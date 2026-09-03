// Command omasend-engine is the headless LocalSend engine behind the
// omarchy-shell omasend plugin. It runs the receiver, discovery, and sender
// continuously and exposes them over a unix socket speaking JSON Lines:
// every line from the engine is an event object, every line from a client is
// a request object. The QML service inside omarchy-shell is the expected
// client, but the protocol is client-agnostic (socat works fine for poking).
//
// The engine never touches the terminal and never raises desktop
// notifications — presentation belongs to the shell.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"omasend/internal/client"
	"omasend/internal/config"
	"omasend/internal/dbg"
	"omasend/internal/discovery"
	"omasend/internal/remotes"
	"omasend/internal/server"
	"omasend/internal/transfer"
)

// ---------------------------------------------------------------- wire types

// event is the envelope for every engine→client line. Only the fields
// relevant to the named event are populated.
type event struct {
	Event string `json:"event"`

	// ready / status
	Alias       string `json:"alias,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Port        int    `json:"port,omitempty"`
	ReceiveDir  string `json:"receiveDir,omitempty"`
	AutoAccept  *bool  `json:"autoAccept,omitempty"`
	PinSet      *bool  `json:"pinSet,omitempty"`

	// peers
	Peers []peerJSON `json:"peers,omitempty"`

	// transfer
	Dir      string `json:"dir,omitempty"`  // "in" | "out"
	Kind     string `json:"kind,omitempty"` // start|progress|filedone|error|cancel
	ID       string `json:"id,omitempty"`
	File     string `json:"file,omitempty"`
	Received int64  `json:"received,omitempty"`
	Total    int64  `json:"total,omitempty"`

	// message (single) and messages (history replay on connect)
	From     string        `json:"from,omitempty"`
	To       string        `json:"to,omitempty"`
	Text     string        `json:"text,omitempty"`
	Time     string        `json:"time,omitempty"`
	Outgoing *bool         `json:"outgoing,omitempty"`
	Messages []messageJSON `json:"messages,omitempty"`

	// offer / offerDone
	OfferID  string     `json:"offerId,omitempty"`
	IP       string     `json:"ip,omitempty"`
	Files    []fileJSON `json:"files,omitempty"`
	Accepted *bool      `json:"accepted,omitempty"`

	// sendResult
	Seq         int64  `json:"seq,omitempty"`
	OK          *bool  `json:"ok,omitempty"`
	Error       string `json:"error,omitempty"`
	PinRequired bool   `json:"pinRequired,omitempty"`
}

type peerJSON struct {
	Alias       string `json:"alias"`
	IP          string `json:"ip"`
	Model       string `json:"model,omitempty"`
	Type        string `json:"type,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	LastSeen    string `json:"lastSeen"`
}

type fileJSON struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type messageJSON struct {
	From     string `json:"from"`
	To       string `json:"to,omitempty"`
	Text     string `json:"text"`
	Time     string `json:"time"`
	Outgoing bool   `json:"outgoing"`
}

// request is every client→engine line.
type request struct {
	Req string `json:"req"`
	Seq int64  `json:"seq,omitempty"`

	// send
	To      string   `json:"to,omitempty"`
	IP      string   `json:"ip,omitempty"`
	Message string   `json:"message,omitempty"`
	Paths   []string `json:"paths,omitempty"`
	Pin     string   `json:"pin,omitempty"`

	// accept
	OfferID string `json:"offerId,omitempty"`
	Accept  *bool  `json:"accept,omitempty"`

	// set (all optional; only present fields change)
	Alias      *string `json:"alias,omitempty"`
	SetPin     *string `json:"setPin,omitempty"`
	ReceiveDir *string `json:"receiveDir,omitempty"`
	AutoAccept *bool   `json:"autoAccept,omitempty"`

	// addPeer
	Host string `json:"host,omitempty"`
}

// ---------------------------------------------------------------- hub

// hub owns the socket listener and fans events out to every connected client.
type hub struct {
	mu      sync.Mutex
	clients map[*hubClient]bool
}

type hubClient struct {
	conn net.Conn
	out  chan []byte
}

func newHub() *hub { return &hub{clients: map[*hubClient]bool{}} }

// broadcast marshals ev and queues it on every client. A client whose queue
// is full is dropped rather than allowed to stall the engine.
func (h *hub) broadcast(ev event) {
	line, err := json.Marshal(ev)
	if err != nil {
		return
	}
	line = append(line, '\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c.out <- line:
		default:
			delete(h.clients, c)
			close(c.out)
		}
	}
}

// sendTo queues a line for one client if it is still a member. broadcast may
// drop a stalled client and close its queue at any time; membership and that
// close both happen under mu, so checking membership here makes this send
// race-free (a bare send on c.out could hit a closed channel and panic).
func (h *hub) sendTo(c *hubClient, ev event) {
	line, err := json.Marshal(ev)
	if err != nil {
		return
	}
	line = append(line, '\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.clients[c] {
		return
	}
	select {
	case c.out <- line:
	default:
		delete(h.clients, c)
		close(c.out)
	}
}

func (h *hub) add(c *hubClient) {
	h.mu.Lock()
	h.clients[c] = true
	h.mu.Unlock()
}

func (h *hub) remove(c *hubClient) {
	h.mu.Lock()
	if h.clients[c] {
		delete(h.clients, c)
		close(c.out)
	}
	h.mu.Unlock()
}

// ---------------------------------------------------------------- engine

type engine struct {
	cfgMu  sync.Mutex
	cfg    config.Config
	disc   *discovery.Discoverer
	srv    *server.Server
	sender *client.Sender
	rem    *remotes.Set
	hub    *hub

	offerMu sync.Mutex
	offers  map[string]server.AcceptRequest
	offerID int64

	msgMu    sync.Mutex
	messages []messageJSON // ring buffer, newest last
}

const messageHistoryCap = 200

func boolPtr(b bool) *bool { return &b }

// statusEvent snapshots identity + settings for ready/status broadcasts.
func (e *engine) statusEvent(name string) event {
	e.cfgMu.Lock()
	defer e.cfgMu.Unlock()
	return event{
		Event:       name,
		Alias:       e.cfg.Alias,
		Fingerprint: e.cfg.Fingerprint,
		Port:        e.cfg.Port,
		ReceiveDir:  e.cfg.ReceiveDir,
		AutoAccept:  boolPtr(e.cfg.AutoAccept),
		PinSet:      boolPtr(e.cfg.PIN != ""),
	}
}

func (e *engine) peersEvent() event {
	snap := e.disc.Snapshot()
	peers := make([]peerJSON, 0, len(snap))
	for _, p := range snap {
		peers = append(peers, peerJSON{
			Alias:       p.Info.Alias,
			IP:          p.IP,
			Model:       p.Info.DeviceModel,
			Type:        string(p.Info.DeviceType),
			Fingerprint: p.Info.Fingerprint,
			LastSeen:    p.LastSeen.Format(time.RFC3339),
		})
	}
	ev := event{Event: "peers", Peers: peers}
	if ev.Peers == nil {
		ev.Peers = []peerJSON{}
	}
	return ev
}

func (e *engine) recordMessage(m messageJSON) {
	e.msgMu.Lock()
	e.messages = append(e.messages, m)
	if len(e.messages) > messageHistoryCap {
		e.messages = e.messages[len(e.messages)-messageHistoryCap:]
	}
	e.msgMu.Unlock()
}

func (e *engine) messageHistory() []messageJSON {
	e.msgMu.Lock()
	defer e.msgMu.Unlock()
	out := make([]messageJSON, len(e.messages))
	copy(out, e.messages)
	return out
}

// pumpEvents forwards the service channels to the hub for the life of ctx.
func (e *engine) pumpEvents(ctx context.Context) {
	go func() { // discovery -> peers snapshots
		for {
			select {
			case <-ctx.Done():
				return
			case <-e.disc.Events():
				e.hub.broadcast(e.peersEvent())
			}
		}
	}()
	go func() { // received messages
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-e.srv.Messages():
				mj := messageJSON{From: m.From, Text: m.Text, Time: m.Time.Format(time.RFC3339)}
				e.recordMessage(mj)
				e.hub.broadcast(event{Event: "message", From: mj.From, Text: mj.Text, Time: mj.Time, Outgoing: boolPtr(false)})
			}
		}
	}()
	go func() { // accept requests -> offers
		for {
			select {
			case <-ctx.Done():
				return
			case req := <-e.srv.Accepts():
				e.holdOffer(ctx, req)
			}
		}
	}()
	go e.pumpTransfers(ctx, e.srv.Transfers(), "in")
	go e.pumpTransfers(ctx, e.sender.Events(), "out")
}

// pumpTransfers forwards transfer events, throttling Progress to one line per
// transfer ID per 100ms so a fast link doesn't flood the socket.
func (e *engine) pumpTransfers(ctx context.Context, ch <-chan transfer.Event, dir string) {
	lastProgress := map[string]time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-ch:
			kind := ""
			switch ev.Kind {
			case transfer.Start:
				kind = "start"
			case transfer.Progress:
				kind = "progress"
				if t, ok := lastProgress[ev.ID]; ok && time.Since(t) < 100*time.Millisecond {
					continue
				}
				lastProgress[ev.ID] = time.Now()
			case transfer.FileDone:
				kind = "filedone"
				delete(lastProgress, ev.ID)
			case transfer.Error:
				kind = "error"
				delete(lastProgress, ev.ID)
			case transfer.Cancel:
				kind = "cancel"
				delete(lastProgress, ev.ID)
			}
			out := event{
				Event: "transfer", Dir: dir, Kind: kind, ID: ev.ID,
				File: ev.FileName, Received: ev.Received, Total: ev.Total,
			}
			if ev.Err != nil {
				out.Error = ev.Err.Error()
			}
			e.hub.broadcast(out)
		}
	}
}

// maxHeldOffers bounds how many pre-accept offers may be parked at once.
// Each parked offer retains the sender's decoded file metadata (itself
// bounded by the prepare-upload body cap) for up to 55 seconds, so without
// this the offer stage would be remote-driven unbounded memory — the
// session caps only apply after an accept. Past the cap new offers are
// declined immediately; no user is going to answer nine simultaneous
// prompts anyway.
const maxHeldOffers = 8

// holdOffer parks an incoming accept request, tells the clients, and waits for
// an accept/decline reply (or times out declining). Runs in its own goroutine
// so the server's prepare-upload handler keeps blocking on req.Reply.
func (e *engine) holdOffer(ctx context.Context, req server.AcceptRequest) {
	e.offerMu.Lock()
	if len(e.offers) >= maxHeldOffers {
		e.offerMu.Unlock()
		req.Reply <- server.AcceptDecision{Accept: false}
		return
	}
	e.offerID++
	id := "offer-" + strconv.FormatInt(e.offerID, 10)
	e.offers[id] = req
	e.offerMu.Unlock()

	files := make([]fileJSON, 0, len(req.Files))
	for _, f := range req.Files {
		files = append(files, fileJSON{Name: f.FileName, Size: f.Size})
	}
	e.hub.broadcast(event{
		Event: "offer", OfferID: id, From: req.From.Alias, IP: req.IP,
		Total: req.TotalSize, Files: files,
	})

	go func() {
		// Must undercut the server's own 60s accept wait: past that the
		// transfer is already declined and an accept would go nowhere.
		t := time.NewTimer(55 * time.Second)
		defer t.Stop()
		select {
		case <-ctx.Done():
			e.resolveOffer(id, false)
		case <-t.C:
			e.resolveOffer(id, false)
		}
	}()
}

// pendingOfferEvents snapshots the parked offers as offer events, for replay
// to a freshly connected client.
func (e *engine) pendingOfferEvents() []event {
	e.offerMu.Lock()
	defer e.offerMu.Unlock()
	out := make([]event, 0, len(e.offers))
	for id, req := range e.offers {
		files := make([]fileJSON, 0, len(req.Files))
		for _, f := range req.Files {
			files = append(files, fileJSON{Name: f.FileName, Size: f.Size})
		}
		out = append(out, event{
			Event: "offer", OfferID: id, From: req.From.Alias, IP: req.IP,
			Total: req.TotalSize, Files: files,
		})
	}
	return out
}

// resolveOffer answers a parked offer exactly once and tells the clients.
func (e *engine) resolveOffer(id string, accept bool) {
	e.offerMu.Lock()
	req, ok := e.offers[id]
	if ok {
		delete(e.offers, id)
	}
	e.offerMu.Unlock()
	if !ok {
		return // already resolved (client reply vs timeout race)
	}
	req.Reply <- server.AcceptDecision{Accept: accept}
	e.hub.broadcast(event{Event: "offerDone", OfferID: id, Accepted: boolPtr(accept)})
}

// findPeer resolves a send target: explicit IP first, then case-insensitive
// alias over discovery, waiting up to wait for it to appear.
func (e *engine) findPeer(ctx context.Context, to, ip string, wait time.Duration) (discovery.Peer, error) {
	fctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	if ip != "" {
		_ = e.disc.Probe(fctx, ip)
	}
	want := strings.ToLower(strings.TrimSpace(to))
	return e.disc.FindPeer(fctx, func(p discovery.Peer) bool {
		if ip != "" {
			return p.IP == ip
		}
		return strings.ToLower(strings.TrimSpace(p.Info.Alias)) == want
	})
}

// handleSend performs a message and/or file send and reports one sendResult
// carrying the request's seq. Runs in its own goroutine per request.
func (e *engine) handleSend(ctx context.Context, req request) {
	fail := func(err error) {
		e.hub.broadcast(event{
			Event: "sendResult", Seq: req.Seq, OK: boolPtr(false),
			Error: err.Error(), PinRequired: errors.Is(err, transfer.ErrPinRequired),
		})
	}
	if req.To == "" && req.IP == "" {
		fail(fmt.Errorf("send needs a target alias or ip"))
		return
	}
	if req.Message == "" && len(req.Paths) == 0 {
		fail(fmt.Errorf("send needs a message, paths, or both"))
		return
	}
	peer, err := e.findPeer(ctx, req.To, req.IP, 15*time.Second)
	if err != nil {
		fail(fmt.Errorf("peer not found: %s", strings.TrimSpace(req.To+" "+req.IP)))
		return
	}
	if req.Message != "" {
		if err := e.sender.SendMessageSync(peer, req.Message, req.Pin); err != nil {
			fail(err)
			return
		}
		mj := messageJSON{
			From: "me", To: peer.Info.Alias, Text: req.Message,
			Time: time.Now().Format(time.RFC3339), Outgoing: true,
		}
		e.recordMessage(mj)
		e.hub.broadcast(event{Event: "message", From: mj.From, To: mj.To, Text: mj.Text, Time: mj.Time, Outgoing: boolPtr(true)})
	}
	if len(req.Paths) > 0 {
		paths := make([]string, 0, len(req.Paths))
		for _, p := range req.Paths {
			paths = append(paths, config.ExpandHome(p))
		}
		if err := e.sender.SendFilesSync(ctx, peer, paths, req.Pin, nil); err != nil {
			fail(err)
			return
		}
	}
	e.hub.broadcast(event{Event: "sendResult", Seq: req.Seq, OK: boolPtr(true), To: peer.Info.Alias})
}

// handleSet applies partial settings, pushes them into the live services, and
// persists the config. Broadcasts a fresh status afterwards.
func (e *engine) handleSet(req request) {
	e.cfgMu.Lock()
	if req.Alias != nil && strings.TrimSpace(*req.Alias) != "" {
		e.cfg.Alias = strings.TrimSpace(*req.Alias)
		e.disc.SetAlias(e.cfg.Alias)
		e.srv.SetAlias(e.cfg.Alias)
		e.sender.SetAlias(e.cfg.Alias)
		e.disc.Announce()
	}
	if req.SetPin != nil {
		e.cfg.PIN = *req.SetPin
		e.srv.SetPIN(e.cfg.PIN)
	}
	if req.ReceiveDir != nil && strings.TrimSpace(*req.ReceiveDir) != "" {
		e.cfg.ReceiveDir = config.ExpandHome(*req.ReceiveDir)
		if err := os.MkdirAll(e.cfg.ReceiveDir, 0o755); err != nil {
			log.Printf("receive dir: %v", err)
		}
		e.srv.SetReceiveDir(e.cfg.ReceiveDir)
	}
	if req.AutoAccept != nil {
		e.cfg.AutoAccept = *req.AutoAccept
		e.srv.SetAutoAccept(e.cfg.AutoAccept)
	}
	if err := e.cfg.Save(); err != nil {
		log.Printf("config save: %v", err)
	}
	e.cfgMu.Unlock()
	e.hub.broadcast(e.statusEvent("status"))
}

func (e *engine) handleAddPeer(ctx context.Context, host string) {
	host = strings.TrimSpace(host)
	if host == "" {
		return
	}
	if e.rem.Add(host) {
		e.cfgMu.Lock()
		e.cfg.KnownPeers = append(e.cfg.KnownPeers, host)
		if err := e.cfg.Save(); err != nil {
			log.Printf("config save: %v", err)
		}
		e.cfgMu.Unlock()
	}
	go func() {
		pctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		_ = e.disc.Probe(pctx, host)
		e.hub.broadcast(e.peersEvent())
	}()
}

// serveClient runs one connection: greet with ready + peers + message history,
// then loop decoding request lines.
func (e *engine) serveClient(ctx context.Context, conn net.Conn) {
	c := &hubClient{conn: conn, out: make(chan []byte, 256)}
	e.hub.add(c)

	// Writer: drains the queue; exits when the hub closes the channel or the
	// connection breaks.
	go func() {
		for line := range c.out {
			if _, err := conn.Write(line); err != nil {
				break
			}
		}
		conn.Close()
	}()

	greet := func(ev event) { e.hub.sendTo(c, ev) }
	greet(e.statusEvent("ready"))
	greet(e.peersEvent())
	greet(event{Event: "messages", Messages: e.messageHistory()})
	// Replay offers still parked from before this client connected (a shell
	// restart inside the accept window must not lose the prompt).
	for _, ev := range e.pendingOfferEvents() {
		greet(ev)
	}

	// One request per line, parsed individually: a malformed or mistyped line
	// is skipped rather than tearing down the connection (a streaming Decoder
	// cannot resync after an error, which dropped the client on any stray
	// byte). Generous line cap for long staged-path lists.
	scan := bufio.NewScanner(conn)
	scan.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scan.Scan() {
		line := scan.Bytes()
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			log.Printf("client: skipping bad line (%v)", err)
			continue
		}
		switch req.Req {
		case "send":
			go e.handleSend(ctx, req)
		case "accept":
			e.resolveOffer(req.OfferID, req.Accept != nil && *req.Accept)
		case "set":
			e.handleSet(req)
		case "addPeer":
			e.handleAddPeer(ctx, req.Host)
		case "status":
			greet(e.statusEvent("status"))
		case "peers":
			greet(e.peersEvent())
		}
	}
	e.hub.remove(c)
	conn.Close()
}

// ---------------------------------------------------------------- main

// socketPath returns the default control-socket path for this user. The
// socket must live in a directory only this user can reach: a predictable
// name in a shared directory like /tmp can be pre-created by another local
// user, whose fake engine would then receive every PIN, path and message
// the shell sends. XDG_RUNTIME_DIR is per-user 0700 by contract; without
// it the fallback is a 0700 directory under the user's own home — never
// /tmp, and if no private place can be established the engine refuses to
// start rather than degrade.
func socketPath() (string, error) {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "omasend.sock"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no XDG_RUNTIME_DIR and no home directory: %w (pass --socket explicitly)", err)
	}
	dir := filepath.Join(home, ".local", "state", "omasend")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("socket dir: %w", err)
	}
	// MkdirAll leaves an existing directory's mode alone and follows a
	// symlinked final component, so verify what is actually there: a real
	// directory, owned by this user, no access for anyone else.
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("socket dir: %w", err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("socket dir %s is not a directory", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid != uint32(os.Getuid()) {
		return "", fmt.Errorf("socket dir %s is not owned by uid %d", dir, os.Getuid())
	}
	if fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return "", fmt.Errorf("socket dir: %w", err)
		}
	}
	return filepath.Join(dir, "omasend.sock"), nil
}

// prepareSocket clears a stale socket file left by a crashed engine, but
// never one a live engine is serving: it dials first, and only a refused
// connection is treated as stale. Both paths this runs on are private to
// the user, so the remove cannot be steered by anyone else.
func prepareSocket(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil // nothing there — the normal case
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket — refusing to remove it", path)
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err == nil {
		conn.Close()
		return fmt.Errorf("another engine is already serving %s", path)
	}
	// Only a refused connection proves no listener holds the socket. Any
	// other dial outcome — a timeout against a busy engine, a permission
	// error — is ambiguous, and removing on it could unlink a live engine's
	// socket. Refuse to start instead of guessing.
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("cannot tell whether %s is live (dial: %v) — not removing it; retry, or remove it yourself if the engine is gone", path, err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("stale socket: %w", err)
	}
	return nil
}

// sameUser reports whether the peer on a unix-socket connection is this
// same uid (or root). The private socket directory already gates access;
// this closes the door independently of any directory-permission mistake.
func sameUser(conn net.Conn) bool {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return false
	}
	var cred *syscall.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil || cerr != nil || cred == nil {
		return false
	}
	return cred.Uid == uint32(os.Getuid()) || cred.Uid == 0
}

func main() {
	var (
		sockFlag = flag.String("socket", "", "control socket path (default: a per-user private location)")
		portFlag = flag.Int("port", 0, "listen port (overrides config for this run)")
	)
	flag.Parse()
	log.SetOutput(dbg.Writer())

	if *sockFlag == "" {
		p, err := socketPath()
		if err != nil {
			fmt.Fprintf(os.Stderr, "socket: %v\n", err)
			os.Exit(1)
		}
		*sockFlag = p
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	if *portFlag != 0 {
		cfg.Port = *portFlag
	}

	// Make sure the download folder exists up front, so the first receive
	// never races directory creation and the folder is browsable immediately.
	if err := os.MkdirAll(cfg.ReceiveDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "receive dir: %v\n", err)
	}

	// Single-instance and stale-socket handling, before anything binds a
	// network port: prepareSocket refuses a live engine or a non-socket file
	// at the path, and removes only a socket nothing answered on. (An
	// unguarded dial-then-remove used to sit here — it could delete a
	// non-socket file and, on a transient dial failure, unlink a live
	// engine's socket.)
	if err := prepareSocket(*sockFlag); err != nil {
		fmt.Fprintf(os.Stderr, "socket: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var cert *tls.Certificate
	if cfg.Protocol == "https" {
		c, err := cfg.TLSCertificate()
		if err != nil {
			fmt.Fprintf(os.Stderr, "tls: %v\n", err)
			os.Exit(1)
		}
		cert = &c
	}

	disc := discovery.New(cfg.DeviceInfo(), cert)

	srv := server.New(server.Options{
		Info:       cfg.DeviceInfo(),
		OnPeer:     disc.NotePeer,
		Cert:       cert,
		ReceiveDir: cfg.ReceiveDir,
		AutoAccept: cfg.AutoAccept,
		PIN:        cfg.PIN,
	})
	if err := srv.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "server: %v\n", err)
		os.Exit(1)
	}
	if err := disc.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "discovery: %v\n", err)
		os.Exit(1)
	}

	sender := client.New(cfg.DeviceInfo(), cert)
	rem := remotes.NewSet(cfg.KnownPeers)
	go remotes.Watch(ctx, disc, rem)

	eng := &engine{
		cfg: cfg, disc: disc, srv: srv, sender: sender, rem: rem,
		hub: newHub(), offers: map[string]server.AcceptRequest{},
	}
	eng.pumpEvents(ctx)
	disc.Announce()

	ln, err := net.Listen("unix", *sockFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "socket: %v\n", err)
		os.Exit(1)
	}
	// The directory is the real gate; the socket mode is depth on top.
	if err := os.Chmod(*sockFlag, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "socket: %v\n", err)
		os.Exit(1)
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		cancel()
		ln.Close()
		os.Remove(*sockFlag)
		os.Exit(0)
	}()

	fmt.Printf("omasend-engine: %s (%s) on port %d, socket %s\n",
		cfg.Alias, cfg.Fingerprint[:min(12, len(cfg.Fingerprint))], cfg.Port, *sockFlag)

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		if !sameUser(conn) {
			conn.Close()
			continue
		}
		go eng.serveClient(ctx, conn)
	}
}
