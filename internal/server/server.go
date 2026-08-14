// Package server hosts the receiver-side LocalSend HTTP API: discovery
// (/info, /register) plus the upload flow (/prepare-upload, /upload, /cancel).
package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"omasend/internal/dbg"
	"omasend/internal/protocol"
	"omasend/internal/transfer"
)

// PeerSink records a peer learned from an inbound request (e.g. /register).
type PeerSink func(info protocol.DeviceInfo, ip string)

// Options configures a Server.
type Options struct {
	Info       protocol.DeviceInfo
	OnPeer     PeerSink         // optional; called when a peer registers with us
	Cert       *tls.Certificate // if set, serve TLS (HTTPS / encrypted mode)
	ReceiveDir string           // where incoming files are written
	AutoAccept bool             // skip the accept prompt if true
	PIN        string           // if non-empty, senders must supply this PIN
}

// Server serves the LocalSend HTTP API for this device.
type Server struct {
	opts     Options
	http     *http.Server
	sessions *sessionStore

	autoAccept atomic.Bool // runtime-toggleable

	// mu guards the runtime-mutable settings below.
	mu         sync.Mutex
	info       protocol.DeviceInfo
	receiveDir string
	pin        string

	accepts   chan AcceptRequest
	transfers chan transfer.Event
	messages  chan ReceivedMessage
}

// ReceivedMessage is a plain-text message received from a peer (LocalSend
// "send message": a single text file whose content rides in the preview field).
type ReceivedMessage struct {
	From string
	Text string
	Time time.Time
}

// New returns a Server from the given options.
func New(opts Options) *Server {
	s := &Server{
		opts:       opts,
		info:       opts.Info,
		receiveDir: opts.ReceiveDir,
		pin:        opts.PIN,
		sessions:   newSessionStore(),
		accepts:    make(chan AcceptRequest, 8),
		transfers:  make(chan transfer.Event, 256),
		messages:   make(chan ReceivedMessage, 32),
	}
	s.autoAccept.Store(opts.AutoAccept)
	mux := http.NewServeMux()
	mux.HandleFunc(protocol.PathInfo, s.handleInfo)
	mux.HandleFunc(protocol.PathRegister, s.handleRegister)
	mux.HandleFunc(protocol.PathPrepareUpload, s.handlePrepareUpload)
	mux.HandleFunc(protocol.PathUpload, s.handleUpload)
	mux.HandleFunc(protocol.PathCancel, s.handleCancel)
	s.http = &http.Server{
		Addr:              fmt.Sprintf(":%d", opts.Info.Port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if opts.Cert != nil {
		s.http.TLSConfig = &tls.Config{Certificates: []tls.Certificate{*opts.Cert}}
	}
	return s
}

// SetAutoAccept toggles whether incoming transfers skip the accept prompt.
func (s *Server) SetAutoAccept(v bool) { s.autoAccept.Store(v) }

// AutoAccept reports the current auto-accept state.
func (s *Server) AutoAccept() bool { return s.autoAccept.Load() }

// SetAlias updates the alias advertised by /info and /register at runtime.
func (s *Server) SetAlias(alias string) {
	s.mu.Lock()
	s.info.Alias = alias
	s.info.DeviceModel = alias
	s.mu.Unlock()
}

// SetReceiveDir updates where incoming files are written at runtime.
func (s *Server) SetReceiveDir(dir string) {
	s.mu.Lock()
	s.receiveDir = dir
	s.mu.Unlock()
}

// SetPIN updates the required PIN at runtime ("" disables it).
func (s *Server) SetPIN(pin string) {
	s.mu.Lock()
	s.pin = pin
	s.mu.Unlock()
}

func (s *Server) infoCopy() protocol.DeviceInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info
}

// Accepts returns the channel of incoming upload requests awaiting a decision.
func (s *Server) Accepts() <-chan AcceptRequest { return s.accepts }

// Transfers returns the channel of incoming-transfer progress events.
func (s *Server) Transfers() <-chan transfer.Event { return s.transfers }

// Messages returns the channel of received plain-text messages.
func (s *Server) Messages() <-chan ReceivedMessage { return s.messages }

// Start binds the listener and serves in the background until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.http.Shutdown(shutCtx)
	}()
	if s.opts.Cert != nil {
		go func() { _ = s.http.ServeTLS(ln, "", "") }() // cert already in TLSConfig
	} else {
		go func() { _ = s.http.Serve(ln) }()
	}
	return nil
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.infoCopy())
}

// handleRegister records the calling peer and replies with our own info.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if s.opts.OnPeer != nil {
		var info protocol.DeviceInfo
		if err := json.NewDecoder(r.Body).Decode(&info); err == nil && info.Fingerprint != "" {
			dbg.Logf("register from %s: alias=%q proto=%s port=%d", clientIP(r), info.Alias, info.Protocol, info.Port)
			s.opts.OnPeer(info, clientIP(r))
		} else if err != nil {
			dbg.Logf("register from %s: decode error: %v", clientIP(r), err)
		}
	}
	writeJSON(w, s.infoCopy())
}

// handlePrepareUpload asks the user to accept, then issues a session + tokens.
func (s *Server) handlePrepareUpload(w http.ResponseWriter, r *http.Request) {
	var req protocol.PrepareUploadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(req.Files) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if meta, err := json.Marshal(req.Files); err == nil {
		dbg.Logf("prepare-upload from %s: alias=%q files=%s", clientIP(r), req.Info.Alias, string(meta))
	}

	// PIN gate: when configured, the sender must supply a matching ?pin=.
	s.mu.Lock()
	pin := s.pin
	s.mu.Unlock()
	if pin != "" && r.URL.Query().Get("pin") != pin {
		dbg.Logf("prepare-upload from %s: PIN missing/incorrect -> 401", clientIP(r))
		http.Error(w, "pin required", http.StatusUnauthorized)
		return
	}

	// A "message" is a single text file whose content rides in the preview
	// field (LocalSend convention). It's received in full right here — surface
	// it and return an empty file set so the sender uploads nothing.
	if text, ok := messageOf(req.Files); ok {
		s.emitMessage(ReceivedMessage{From: req.Info.Alias, Text: text, Time: time.Now()})
		writeJSON(w, protocol.PrepareUploadResponse{SessionID: randToken(), Files: map[string]string{}})
		return
	}

	if !s.askAccept(req, clientIP(r)) {
		http.Error(w, "rejected", http.StatusForbidden)
		return
	}

	sess, tokens := s.sessions.create(req.Info, clientIP(r), req.Files)
	writeJSON(w, protocol.PrepareUploadResponse{SessionID: sess.id, Files: tokens})
}

// messageOf reports whether files represents a plain-text message (exactly one
// text file with non-empty preview) and returns the message text.
func messageOf(files map[string]protocol.FileMetadata) (string, bool) {
	if len(files) != 1 {
		return "", false
	}
	for _, f := range files {
		if f.Preview != "" && isTextType(f.FileType) {
			return f.Preview, true
		}
	}
	return "", false
}

// isTextType matches both the MIME form ("text/plain") that LocalSend sends and
// the bare enum form ("text") older clients may use.
func isTextType(fileType string) bool {
	return fileType == "text" || strings.HasPrefix(fileType, "text/")
}

// emitMessage delivers a received message without blocking the HTTP handler.
func (s *Server) emitMessage(m ReceivedMessage) {
	dbg.Logf("received message from %q: %q", m.From, m.Text)
	select {
	case s.messages <- m:
	default:
	}
}

// askAccept honours auto-accept, or raises an AcceptRequest and blocks for the
// user's decision (with a timeout so a never-answered prompt can't wedge a
// peer's HTTP connection forever).
func (s *Server) askAccept(req protocol.PrepareUploadRequest, ip string) bool {
	if s.autoAccept.Load() {
		return true
	}
	var total int64
	for _, f := range req.Files {
		total += f.Size
	}
	reply := make(chan AcceptDecision, 1)
	ar := AcceptRequest{From: req.Info, IP: ip, Files: req.Files, TotalSize: total, Reply: reply}
	select {
	case s.accepts <- ar:
	case <-time.After(2 * time.Second):
		return false // nobody draining the prompt channel
	}
	select {
	case d := <-reply:
		return d.Accept
	case <-time.After(60 * time.Second):
		return false
	}
}

// handleUpload validates the token and streams the body to the receive dir.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sessionID, fileID, token := q.Get("sessionId"), q.Get("fileId"), q.Get("token")

	sess, fe, ok := s.sessions.lookup(sessionID, fileID, token)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	key := sessionID + ":" + fileID
	dest, err := s.writeFile(sess, fe, key, r.Body)
	if err != nil {
		s.transfers <- transfer.Event{Dir: transfer.Incoming, Kind: transfer.Error, ID: key, FileName: fe.meta.FileName, Err: err}
		http.Error(w, "write failed", http.StatusInternalServerError)
		return
	}
	dbg.Logf("received %q -> %s", fe.meta.FileName, dest)
	s.transfers <- transfer.Event{Dir: transfer.Incoming, Kind: transfer.FileDone, ID: key, FileName: fe.meta.FileName, Received: fe.meta.Size, Total: fe.meta.Size}
	s.sessions.complete(sessionID, fileID)
	w.WriteHeader(http.StatusOK)
}

// writeFile streams r to a uniquely-named file in the receive dir, emitting
// throttled progress events under the transfer key, and returns the final path.
// It writes to a temp file and renames on success so partial transfers never
// masquerade as complete.
func (s *Server) writeFile(sess *session, fe *fileEntry, key string, r io.Reader) (string, error) {
	s.mu.Lock()
	dir := s.receiveDir
	s.mu.Unlock()
	dest, err := destPath(dir, fe.meta.FileName)
	if err != nil {
		return "", err
	}
	tmp := dest + ".part"

	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}

	pr := &progressReader{
		r:     r,
		total: fe.meta.Size,
		ctx:   sess.ctx,
		emit: func(received int64) {
			select {
			case s.transfers <- transfer.Event{Dir: transfer.Incoming, Kind: transfer.Progress, ID: key, FileName: fe.meta.FileName, Received: received, Total: fe.meta.Size}:
			default:
			}
		},
	}
	s.transfers <- transfer.Event{Dir: transfer.Incoming, Kind: transfer.Start, ID: key, FileName: fe.meta.FileName, Total: fe.meta.Size}

	_, copyErr := io.Copy(f, pr)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		if copyErr != nil {
			return "", copyErr
		}
		return "", closeErr
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	return dest, nil
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("sessionId")
	s.sessions.cancel(sessionID)
	s.transfers <- transfer.Event{Dir: transfer.Incoming, Kind: transfer.Cancel, ID: sessionID}
	w.WriteHeader(http.StatusOK)
}

// destPath resolves a safe, non-colliding path under dir for the (possibly
// nested) filename, creating parent directories. Sub-paths are honoured so a
// folder send recreates its structure, but any traversal is neutralised:
// cleaning against a leading "/" collapses ".." at the root, and a final
// containment check guarantees the result stays within dir.
func destPath(dir, name string) (string, error) {
	rel := strings.TrimPrefix(filepath.Clean("/"+filepath.ToSlash(name)), "/")
	if rel == "" || rel == "." {
		rel = "file"
	}
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if full != dir && !strings.HasPrefix(full, dir+string(os.PathSeparator)) {
		return "", fmt.Errorf("unsafe destination for %q", name)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", err
	}
	return uniqueAt(full), nil
}

// uniqueAt returns full if free, otherwise inserts " (n)" before the extension
// until it finds an unused name in the same directory.
func uniqueAt(full string) string {
	if _, err := os.Stat(full); os.IsNotExist(err) {
		return full
	}
	d := filepath.Dir(full)
	base := filepath.Base(full)
	ext := filepath.Ext(base)
	stem := base[:len(base)-len(ext)]
	for i := 1; ; i++ {
		cand := filepath.Join(d, fmt.Sprintf("%s (%d)%s", stem, i, ext))
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
}

func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// LocalIPs returns this host's non-loopback IPv4 addresses, for display.
func LocalIPs() []string {
	var out []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		if ip4 := ipnet.IP.To4(); ip4 != nil {
			out = append(out, ip4.String())
		}
	}
	return out
}
