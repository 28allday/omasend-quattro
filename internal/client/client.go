// Package client implements the sender side of the LocalSend upload flow:
// prepare-upload to a peer, then stream each file to /upload, emitting progress.
package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"omasend/internal/dbg"
	"omasend/internal/discovery"
	"omasend/internal/protocol"
	"omasend/internal/security"
	"omasend/internal/transfer"
	"omasend/internal/tsproxy"
)

// errOpen wraps a failure to open a source file, so the send loop can skip just
// that file rather than aborting the whole batch (which it does for peer/network
// errors, where the shared session is dead).
var errOpen = errors.New("open source file")

// inflight tracks one running send so it can be cancelled — e.g. when a newer
// transfer to the same peer supersedes it.
type inflight struct {
	cancel context.CancelFunc
}

// Sender uploads files to peers. Events are delivered on Events().
type Sender struct {
	mu     sync.Mutex
	self   protocol.DeviceInfo
	cert   *tls.Certificate
	http   *http.Client
	events chan transfer.Event
	active map[string]*inflight // in-flight sends keyed by peer IP

	// One client per peer fingerprint. LocalSend certificates are self-signed,
	// so chain validation can never apply; what identifies a peer is the
	// fingerprint it advertises. Pinning to it means an on-path attacker with
	// its own certificate cannot stand in for the device you picked and
	// collect the files, message text or PIN meant for it.
	pinned map[string]*http.Client
}

// New returns a Sender advertising self. If a TLS certificate is provided,
// it is attached to outbound HTTP clients for mutual TLS (mTLS) with iOS/Android peers.
func New(self protocol.DeviceInfo, certs ...*tls.Certificate) *Sender {
	var clientCert *tls.Certificate
	if len(certs) > 0 && certs[0] != nil {
		clientCert = certs[0]
	}
	var tlsCerts []tls.Certificate
	if clientCert != nil {
		tlsCerts = []tls.Certificate{*clientCert}
	}
	return &Sender{
		self: self,
		cert: clientCert,
		http: &http.Client{
			// No overall timeout (large files), but bound the parts that can
			// silently wedge on a vanished peer: connecting, the TLS handshake,
			// and waiting for response headers after the body is sent.
			Timeout: 0,
			Transport: &http.Transport{
				// Proxy env vars + tailnet SOCKS5 auto-detection (see
				// discovery) so transfers also work from userspace-networking
				// Tailscale boxes.
				Proxy: tsproxy.ProxyFunc,
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true,
					Certificates:       tlsCerts,
					GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
						if clientCert != nil {
							return clientCert, nil
						}
						return &tls.Certificate{}, nil
					},
				},
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
			},
		},
		events: make(chan transfer.Event, 256),
		active: make(map[string]*inflight),
		pinned: make(map[string]*http.Client),
	}
}

// clientFor returns an HTTP client that will only complete a TLS handshake
// with a peer presenting the given fingerprint. An empty fingerprint (a plain
// http peer, or one we have not yet identified) falls back to the unpinned
// client — there is nothing to pin to yet.
func (s *Sender) clientFor(fingerprint string) *http.Client {
	want := strings.ToUpper(strings.TrimSpace(fingerprint))
	if want == "" {
		return s.http
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.pinned[want]; ok {
		return c
	}
	var tlsCerts []tls.Certificate
	if s.cert != nil {
		tlsCerts = []tls.Certificate{*s.cert}
	}
	c := &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			Proxy: tsproxy.ProxyFunc,
			TLSClientConfig: &tls.Config{
				// Still no chain validation — these are self-signed by design.
				// The fingerprint check below is what authenticates the peer.
				InsecureSkipVerify: true,
				Certificates:       tlsCerts,
				GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
					if s.cert != nil {
						return s.cert, nil
					}
					return &tls.Certificate{}, nil
				},
				VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
					if len(rawCerts) == 0 {
						return fmt.Errorf("peer presented no certificate")
					}
					got := security.Fingerprint(rawCerts[0])
					if got != want {
						return fmt.Errorf("peer fingerprint mismatch: expected %s, got %s", want, got)
					}
					return nil
				},
			},
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}
	s.pinned[want] = c
	return c
}

// Events returns the outgoing-transfer event channel.
func (s *Sender) Events() <-chan transfer.Event { return s.events }

// SetAlias updates the alias we present to peers when sending, at runtime.
func (s *Sender) SetAlias(alias string) {
	s.mu.Lock()
	s.self.Alias = alias
	s.self.DeviceModel = alias
	s.mu.Unlock()
}

func (s *Sender) selfCopy() protocol.DeviceInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.self
}

// Send uploads the given file paths to peer in a background goroutine. pin may
// be empty; supply it when the peer requires one.
func (s *Sender) Send(peer discovery.Peer, paths []string, pin string) {
	go s.send(peer, paths, pin)
}

// SendMessage sends a plain-text message to peer (LocalSend "send message":
// one text file whose content rides in the preview field, so nothing is
// uploaded). pin may be empty; supply it when the peer requires one. Errors —
// including ErrPinRequired — are reported on Events() as an outgoing Error.
func (s *Sender) SendMessage(peer discovery.Peer, text, pin string) {
	go s.sendMessage(peer, text, pin)
}

func (s *Sender) sendMessage(peer discovery.Peer, text, pin string) {
	if err := s.SendMessageSync(peer, text, pin); err != nil {
		dbg.Logf("send message to %s failed: %v", peer.IP, err)
		s.emit(transfer.Event{Dir: transfer.Outgoing, Kind: transfer.Error, FileName: "message", Err: err})
	}
}

// SendMessageSync sends a plain-text message to peer and blocks until the peer
// has accepted it (or an error occurs), returning that error directly —
// including transfer.ErrPinRequired when the peer needs a PIN. Unlike
// SendMessage it reports nothing on Events(); it exists for the headless
// one-shot send path, where there is no TUI to consume events.
func (s *Sender) SendMessageSync(peer discovery.Peer, text, pin string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	id := randID()
	files := map[string]protocol.FileMetadata{
		id: {
			ID:       id,
			FileName: "message.txt",
			Size:     int64(len(text)),
			FileType: "text/plain",
			Preview:  text,
		},
	}
	_, err := s.prepareUpload(ctx, s.clientFor(peer.Info.Fingerprint), s.url(peer), files, pin)
	return err
}

// SendFilesSync uploads the given file/folder paths to peer and blocks until
// the whole batch is done, returning the error directly — including
// transfer.ErrPinRequired when the peer needs a PIN. Unlike Send it reports
// nothing on Events(); it exists for the headless one-shot send path, where
// there is no TUI to consume events (the progress events uploadFile emits are
// harmlessly dropped). onDone, when non-nil, is called after each file lands,
// so the CLI can print per-file progress lines.
func (s *Sender) SendFilesSync(ctx context.Context, peer discovery.Peer, paths []string, pin string, onDone func(name string, size int64)) error {
	// Explicitly named paths that don't exist are a hard error up front — in
	// the TUI a stat failure is just an event on one staged entry, but a script
	// passing a wrong path wants a non-zero exit, not a silent skip.
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			return err
		}
	}
	items := s.expand(paths)
	if len(items) == 0 {
		return errors.New("nothing to send: no readable files under the given paths")
	}

	files := make(map[string]protocol.FileMetadata, len(items))
	pathByID := make(map[string]string, len(items))
	for _, it := range items {
		id := randID()
		files[id] = protocol.FileMetadata{
			ID:       id,
			FileName: it.name,
			Size:     it.size,
			FileType: mimeType(it.path),
		}
		pathByID[id] = it.path
	}

	base := s.url(peer)
	hc := s.clientFor(peer.Info.Fingerprint)
	prepResp, err := s.prepareUpload(ctx, hc, base, files, pin)
	if err != nil {
		return err
	}

	// An empty token map (204) means the peer accepted but wants nothing
	// uploaded — e.g. it already has the files. That's success: the loop below
	// simply finds no tokens to push.
	var skipped []string
	for id, token := range prepResp.Files {
		meta := files[id]
		key := prepResp.SessionID + ":" + id
		if err := s.uploadFile(ctx, hc, base, prepResp.SessionID, id, token, key, pathByID[id], meta); err != nil {
			// A failure to open a local file is specific to that file (it
			// vanished or lost permissions since staging) — skip it and keep
			// the batch going, like the TUI path does. Anything else means the
			// peer/session is gone and can't be resumed, so abort the batch.
			if errors.Is(err, errOpen) {
				skipped = append(skipped, meta.FileName)
				continue
			}
			s.emit(transfer.Event{Dir: transfer.Outgoing, Kind: transfer.Error, ID: key, FileName: meta.FileName, Err: err})
			return fmt.Errorf("upload %q: %w", meta.FileName, err)
		}
		// Mirror the async path: uploadFile emits Start/Progress but leaves
		// FileDone to its caller. Without this, an Events() consumer (the
		// engine daemon) sees sync-sent files stuck in progress forever.
		s.emit(transfer.Event{Dir: transfer.Outgoing, Kind: transfer.FileDone, ID: key, FileName: meta.FileName, Received: meta.Size, Total: meta.Size})
		if onDone != nil {
			onDone(meta.FileName, meta.Size)
		}
	}
	if len(skipped) > 0 {
		return fmt.Errorf("could not read: %s", strings.Join(skipped, ", "))
	}
	return nil
}

func (s *Sender) send(peer discovery.Peer, paths []string, pin string) {
	// A new transfer to a peer supersedes any still-running one to the same
	// peer: cancel it so a half-finished old batch can't carry on once the user
	// starts something new.
	ctx, cancel := context.WithCancel(context.Background())
	h := &inflight{cancel: cancel}
	s.mu.Lock()
	if prev := s.active[peer.IP]; prev != nil {
		prev.cancel()
	}
	s.active[peer.IP] = h
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		if s.active[peer.IP] == h {
			delete(s.active, peer.IP)
		}
		s.mu.Unlock()
	}()

	// Expand any directories into their files, then build metadata keyed by a
	// generated fileId. A directory's files carry a relative FileName (e.g.
	// "Trip/day1/img.jpg") so the receiver can recreate the folder structure.
	items := s.expand(paths)
	files := make(map[string]protocol.FileMetadata, len(items))
	pathByID := make(map[string]string, len(items))
	for _, it := range items {
		id := randID()
		files[id] = protocol.FileMetadata{
			ID:       id,
			FileName: it.name,
			Size:     it.size,
			FileType: mimeType(it.path),
		}
		pathByID[id] = it.path
	}
	if len(files) == 0 {
		return
	}

	if meta, err := json.Marshal(files); err == nil {
		dbg.Logf("SEND prepare-upload to %s: files=%s", peer.IP, string(meta))
	}
	base := s.url(peer)
	hc := s.clientFor(peer.Info.Fingerprint)
	prepResp, err := s.prepareUpload(ctx, hc, base, files, pin)
	if err != nil {
		dbg.Logf("send prepare-upload to %s failed: %v", peer.IP, err)
		if errors.Is(err, transfer.ErrPinRequired) {
			// One signal is enough for the TUI to prompt + retry.
			s.emit(transfer.Event{Dir: transfer.Outgoing, Kind: transfer.Error, Err: transfer.ErrPinRequired})
			return
		}
		for id, m := range files {
			s.emit(transfer.Event{Dir: transfer.Outgoing, Kind: transfer.Error, ID: id, FileName: m.FileName, Err: err})
		}
		return
	}

	for id, token := range prepResp.Files {
		meta := files[id]
		key := prepResp.SessionID + ":" + id

		// If the transfer was cancelled (superseded, or aborted after an earlier
		// failure), don't push the rest of the batch — report a clean cancel.
		if ctx.Err() != nil {
			s.emit(transfer.Event{Dir: transfer.Outgoing, Kind: transfer.Cancel, ID: key, FileName: meta.FileName})
			continue
		}

		err := s.uploadFile(ctx, hc, base, prepResp.SessionID, id, token, key, pathByID[id], meta)
		if err == nil {
			s.emit(transfer.Event{Dir: transfer.Outgoing, Kind: transfer.FileDone, ID: key, FileName: meta.FileName, Received: meta.Size, Total: meta.Size})
			continue
		}
		dbg.Logf("send upload %q failed: %v", meta.FileName, err)
		if ctx.Err() != nil {
			s.emit(transfer.Event{Dir: transfer.Outgoing, Kind: transfer.Cancel, ID: key, FileName: meta.FileName})
		} else {
			s.emit(transfer.Event{Dir: transfer.Outgoing, Kind: transfer.Error, ID: key, FileName: meta.FileName, Err: err})
		}
		// A failure to open a local file is specific to that file — skip it and
		// keep going. Any other failure means the peer/session is gone, so abort
		// the rest of the batch (the shared session can't be resumed).
		if !errors.Is(err, errOpen) {
			cancel()
		}
	}
}

// fileItem is one concrete file to upload: its path on disk, the relative name
// advertised to the peer (carries folder structure), and its size.
type fileItem struct {
	path string
	name string
	size int64
}

// expand turns the selected paths into a flat list of files. A regular file is
// passed through with its base name. A directory is walked recursively; each
// contained file's advertised name is its path relative to the directory's
// parent, so the selected folder itself is recreated on the receiver (selecting
// "Trip" yields "Trip/day1/img.jpg", …). Unreadable entries are skipped with an
// error event rather than aborting the whole transfer.
func (s *Sender) expand(paths []string) []fileItem {
	var items []fileItem
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			s.emit(transfer.Event{Dir: transfer.Outgoing, Kind: transfer.Error, FileName: filepath.Base(p), Err: err})
			continue
		}
		if !fi.IsDir() {
			items = append(items, fileItem{path: p, name: filepath.Base(p), size: fi.Size()})
			continue
		}
		root := filepath.Dir(filepath.Clean(p)) // parent, so the folder name is kept
		_ = filepath.WalkDir(p, func(fp string, d fs.DirEntry, err error) error {
			if err != nil {
				s.emit(transfer.Event{Dir: transfer.Outgoing, Kind: transfer.Error, FileName: filepath.Base(fp), Err: err})
				return nil // skip this entry, keep walking the rest
			}
			if d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				s.emit(transfer.Event{Dir: transfer.Outgoing, Kind: transfer.Error, FileName: filepath.Base(fp), Err: err})
				return nil
			}
			rel, err := filepath.Rel(root, fp)
			if err != nil {
				rel = filepath.Base(fp)
			}
			items = append(items, fileItem{path: fp, name: filepath.ToSlash(rel), size: info.Size()})
			return nil
		})
	}
	return items
}

func (s *Sender) prepareUpload(ctx context.Context, hc *http.Client, base string, files map[string]protocol.FileMetadata, pin string) (protocol.PrepareUploadResponse, error) {
	reqBody, _ := json.Marshal(protocol.PrepareUploadRequest{Info: s.selfCopy(), Files: files})
	url := base + protocol.PathPrepareUpload
	if pin != "" {
		url += "?pin=" + neturl.QueryEscape(pin)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return protocol.PrepareUploadResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return protocol.PrepareUploadResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return protocol.PrepareUploadResponse{}, transfer.ErrPinRequired
	}
	if resp.StatusCode == http.StatusNoContent {
		// 204: accepted, but nothing to upload — the official LocalSend client
		// returns this for a message (its text rode in the preview field) and
		// for files the peer already has. No session/token map follows, so
		// return an empty response: a message send is done, and a file send
		// simply finds no tokens to push, which is the correct outcome.
		return protocol.PrepareUploadResponse{}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return protocol.PrepareUploadResponse{}, fmt.Errorf("prepare-upload status %d", resp.StatusCode)
	}
	var pr protocol.PrepareUploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return protocol.PrepareUploadResponse{}, err
	}
	return pr, nil
}

func (s *Sender) uploadFile(ctx context.Context, hc *http.Client, base, sessionID, fileID, token, key, path string, meta protocol.FileMetadata) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%w: %v", errOpen, err)
	}
	defer f.Close()

	s.emit(transfer.Event{Dir: transfer.Outgoing, Kind: transfer.Start, ID: key, FileName: meta.FileName, Total: meta.Size})

	pr := &progressReader{
		r:     f,
		total: meta.Size,
		emit: func(sent int64) {
			s.emit(transfer.Event{Dir: transfer.Outgoing, Kind: transfer.Progress, ID: key, FileName: meta.FileName, Received: sent, Total: meta.Size})
		},
	}

	url := fmt.Sprintf("%s%s?sessionId=%s&fileId=%s&token=%s", base, protocol.PathUpload, sessionID, fileID, token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, pr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = meta.Size

	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upload status %d", resp.StatusCode)
	}
	return nil
}

func (s *Sender) url(peer discovery.Peer) string {
	scheme := "https"
	if peer.Info.Protocol == "http" {
		scheme = "http"
	}
	port := peer.Info.Port
	if port == 0 {
		port = protocol.DefaultPort
	}
	return fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(peer.IP, strconv.Itoa(port)))
}

func (s *Sender) emit(ev transfer.Event) {
	select {
	case s.events <- ev:
	default:
	}
}

// builtinMIME maps common extensions to MIME types so a headless server
// without /etc/mime.types still labels photos/videos correctly. (Go's built-in
// table omits .jpg and mislabels .heic as image/heif.)
var builtinMIME = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png",
	".gif": "image/gif", ".webp": "image/webp", ".bmp": "image/bmp",
	".tiff": "image/tiff", ".tif": "image/tiff", ".heic": "image/heic",
	".heif": "image/heif", ".dng": "image/x-adobe-dng", ".svg": "image/svg+xml",
	".mp4": "video/mp4", ".mov": "video/quicktime", ".m4v": "video/x-m4v",
	".mkv": "video/x-matroska", ".webm": "video/webm", ".avi": "video/x-msvideo",
	".mp3": "audio/mpeg", ".m4a": "audio/mp4", ".wav": "audio/wav",
	".flac": "audio/flac", ".ogg": "audio/ogg", ".opus": "audio/opus",
	".pdf": "application/pdf", ".zip": "application/zip", ".txt": "text/plain",
}

// mimeType resolves a file's MIME type, preferring our built-in table, then the
// system table, then a safe default.
func mimeType(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if t, ok := builtinMIME[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}

func randID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
