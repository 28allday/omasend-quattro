// Package discovery implements LocalSend multicast discovery: it announces this
// device on 224.0.0.167:53317 and listens for other devices, emitting peer
// events on a channel. It is Tea-agnostic; the app layer bridges its events
// into Bubble Tea messages.
package discovery

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"omasend/internal/dbg"
	"omasend/internal/protocol"
	"omasend/internal/tsproxy"
)

// EventKind distinguishes peer lifecycle events.
type EventKind int

const (
	PeerFound EventKind = iota
	PeerLost
)

// Peer is a discovered device plus the address we reached it at.
type Peer struct {
	Info     protocol.DeviceInfo
	IP       string
	LastSeen time.Time
}

// Event is emitted on the Discoverer's channel.
type Event struct {
	Kind EventKind
	Peer Peer
}

const (
	// announceInterval is how often we re-announce ourselves.
	announceInterval = 5 * time.Second
	// peerTTL is how long a peer survives without being seen before eviction.
	peerTTL = 20 * time.Second
	// reapInterval is how often we check for stale peers.
	reapInterval = 5 * time.Second
)

// Discoverer announces this device and tracks discovered peers.
type Discoverer struct {
	self   protocol.DeviceInfo
	events chan Event
	client *http.Client
	gaddr  *net.UDPAddr

	mu       sync.Mutex
	conn     *net.UDPConn    // multicast listener (group-bound)
	sendConn *net.UDPConn    // dedicated sender (dialed to the group)
	peers    map[string]Peer // keyed by fingerprint
}

// New returns a Discoverer that advertises self.
func New(self protocol.DeviceInfo) *Discoverer {
	return &Discoverer{
		self:   self,
		events: make(chan Event, 64),
		// Peers use self-signed certs; we don't validate the chain (LocalSend
		// pins the announced fingerprint instead).
		client: &http.Client{
			Timeout: 3 * time.Second,
			Transport: &http.Transport{
				// Proxy env vars are honoured like the default transport, and
				// tailnet destinations are auto-routed through the local
				// tailscaled SOCKS5 proxy on userspace-networking boxes (no
				// TUN), where direct outbound tailnet dials cannot work.
				Proxy:           tsproxy.ProxyFunc,
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
		peers: make(map[string]Peer),
	}
}

// Events returns the channel on which peer events are delivered.
func (d *Discoverer) Events() <-chan Event { return d.events }

// Snapshot returns a copy of the currently-known peers.
func (d *Discoverer) Snapshot() []Peer {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Peer, 0, len(d.peers))
	for _, p := range d.peers {
		out = append(out, p)
	}
	return out
}

// FindPeer waits for a known peer satisfying pred, checking peers already seen
// first and then ones discovered while waiting. It returns the first match, or
// ctx.Err() if the context is cancelled / times out first. It consumes the
// Events() channel, so it must not run concurrently with another Events()
// reader (e.g. the TUI bridge) — it is intended for the headless send path.
func (d *Discoverer) FindPeer(ctx context.Context, pred func(Peer) bool) (Peer, error) {
	for _, p := range d.Snapshot() {
		if pred(p) {
			return p, nil
		}
	}
	for {
		select {
		case <-ctx.Done():
			return Peer{}, ctx.Err()
		case ev, ok := <-d.events:
			if !ok {
				return Peer{}, ctx.Err()
			}
			if ev.Kind == PeerFound && pred(ev.Peer) {
				return ev.Peer, nil
			}
		}
	}
}

// SetAlias updates the advertised alias (and device model) at runtime. Call
// Announce afterwards to push it out immediately.
func (d *Discoverer) SetAlias(alias string) {
	d.mu.Lock()
	d.self.Alias = alias
	d.self.DeviceModel = alias
	d.mu.Unlock()
}

// selfCopy returns the current self info under lock.
func (d *Discoverer) selfCopy() protocol.DeviceInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.self
}

// Run joins the multicast group and starts the listener and announcer
// goroutines. It returns once the socket is bound.
func (d *Discoverer) Run(ctx context.Context) error {
	gaddr, err := net.ResolveUDPAddr("udp4",
		net.JoinHostPort(protocol.MulticastAddr, strconv.Itoa(protocol.MulticastPort)))
	if err != nil {
		return err
	}
	// ListenMulticastUDP sets SO_REUSEADDR, so this coexists with the real
	// LocalSend app (and a second instance) on the same host.
	conn, err := net.ListenMulticastUDP("udp4", nil, gaddr)
	if err != nil {
		return err
	}
	_ = conn.SetReadBuffer(1 << 20)

	// A packet sent via the group-bound listen socket does not loop back to
	// other local members, so announcements go out on a dedicated dialed socket
	// whose source interface the kernel picks (which does loop back correctly).
	sendConn, err := net.DialUDP("udp4", nil, gaddr)
	if err != nil {
		_ = conn.Close()
		return err
	}

	d.mu.Lock()
	d.conn = conn
	d.sendConn = sendConn
	d.gaddr = gaddr
	d.mu.Unlock()

	go d.listen(ctx, conn)
	go d.announceLoop(ctx)
	go d.reapLoop(ctx)

	go func() {
		<-ctx.Done()
		_ = conn.Close()
		_ = sendConn.Close()
	}()
	return nil
}

// Announce multicasts our presence with announce:true, soliciting replies.
func (d *Discoverer) Announce() {
	d.mu.Lock()
	send := d.sendConn
	self := d.self
	d.mu.Unlock()
	if send == nil {
		return
	}
	payload, err := json.Marshal(self.WithAnnounce(true))
	if err != nil {
		return
	}
	_, _ = send.Write(payload)
}

func (d *Discoverer) announceLoop(ctx context.Context) {
	t := time.NewTicker(announceInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.Announce()
		}
	}
}

// reapLoop evicts peers not seen within peerTTL, emitting PeerLost for each.
func (d *Discoverer) reapLoop(ctx context.Context) {
	t := time.NewTicker(reapInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.reapOnce(time.Now())
		}
	}
}

// reapOnce evicts peers whose LastSeen is older than peerTTL relative to now,
// emitting PeerLost for each. Returns the number evicted.
func (d *Discoverer) reapOnce(now time.Time) int {
	var lost []Peer
	d.mu.Lock()
	for fp, p := range d.peers {
		if now.Sub(p.LastSeen) > peerTTL {
			lost = append(lost, p)
			delete(d.peers, fp)
		}
	}
	d.mu.Unlock()
	for _, p := range lost {
		dbg.Logf("peer evicted (stale >%.0fs): alias=%q ip=%s", peerTTL.Seconds(), p.Info.Alias, p.IP)
		d.emit(Event{Kind: PeerLost, Peer: p})
	}
	return len(lost)
}

func (d *Discoverer) listen(ctx context.Context, conn *net.UDPConn) {
	buf := make([]byte, 64*1024)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		var info protocol.DeviceInfo
		if err := json.Unmarshal(buf[:n], &info); err != nil {
			continue
		}
		if info.Fingerprint == "" || info.Fingerprint == d.self.Fingerprint {
			continue // malformed or our own announcement
		}
		ip := src.IP.String()
		dbg.Logf("multicast from %s: alias=%q proto=%s port=%d announce=%v",
			ip, info.Alias, info.Protocol, info.Port, info.Announce)

		// A probe (announce:true) expects a reply with our info (announce:false),
		// sent to the peer's /register so it learns about us reliably.
		if info.Announce != nil && *info.Announce {
			go d.reply(ip, info.Port, info.Protocol)
		}
		d.NotePeer(info, ip)
	}
}

// reply POSTs our device info to a peer's /register endpoint, using the scheme
// the peer advertised (https for encrypted peers, http otherwise).
func (d *Discoverer) reply(ip string, port int, proto string) {
	if port == 0 {
		port = protocol.DefaultPort
	}
	scheme := "https"
	if proto == "http" {
		scheme = "http"
	}
	body, err := json.Marshal(d.selfCopy().WithAnnounce(false))
	if err != nil {
		return
	}
	url := fmt.Sprintf("%s://%s/api/localsend/v2/register", scheme, net.JoinHostPort(ip, strconv.Itoa(port)))
	resp, err := d.client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		dbg.Logf("reply POST %s FAILED: %v", url, err)
		return
	}
	dbg.Logf("reply POST %s -> %s", url, resp.Status)
	_ = resp.Body.Close()
}

// Probe contacts host directly over unicast — bypassing multicast — and records
// it as a peer on success. It POSTs our info to the peer's /register (so the
// peer also learns us) and reads the peer's info from the reply. host may carry
// a port; otherwise the default LocalSend port is used. https is tried first,
// then http. Used for known/remote peers (e.g. reached over Tailscale) that
// multicast can't find. Re-probing a live peer refreshes its LastSeen so it is
// not reaped; a peer that stops answering ages out normally.
func (d *Discoverer) Probe(ctx context.Context, host string) error {
	h, port := hostPort(host)
	body, err := json.Marshal(d.selfCopy().WithAnnounce(false))
	if err != nil {
		return err
	}
	var lastErr error
	for _, scheme := range []string{"https", "http"} {
		url := fmt.Sprintf("%s://%s/api/localsend/v2/register", scheme, net.JoinHostPort(h, strconv.Itoa(port)))
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := d.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		var info protocol.DeviceInfo
		derr := json.NewDecoder(resp.Body).Decode(&info)
		_ = resp.Body.Close()
		if derr != nil || info.Fingerprint == "" {
			lastErr = fmt.Errorf("probe %s: no usable device info", url)
			continue
		}
		dbg.Logf("probe %s -> alias=%q fp=%s", url, info.Alias, info.Fingerprint)
		d.NotePeer(info, h) // reach it back at the host we dialed
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("could not reach %s", host)
	}
	return lastErr
}

// isLoopback reports whether ip parses as a loopback address (e.g. 127.0.0.1).
func isLoopback(ip string) bool {
	p := net.ParseIP(ip)
	return p != nil && p.IsLoopback()
}

// hostPort splits an optional :port off host, defaulting to the LocalSend port.
// It handles bare IPv6 by requiring the [::]:port form for a custom port.
func hostPort(host string) (string, int) {
	if h, p, err := net.SplitHostPort(host); err == nil {
		if n, err := strconv.Atoi(p); err == nil {
			return h, n
		}
		return h, protocol.DefaultPort
	}
	return host, protocol.DefaultPort
}

// NotePeer records a peer (from multicast or from an inbound /register) and
// emits PeerFound on first sight or when its address changes. Safe for
// concurrent use; ignores our own fingerprint.
func (d *Discoverer) NotePeer(info protocol.DeviceInfo, ip string) {
	if info.Fingerprint == "" || info.Fingerprint == d.self.Fingerprint {
		return
	}
	peer := Peer{Info: info, IP: ip, LastSeen: time.Now()}

	d.mu.Lock()
	prev, existed := d.peers[info.Fingerprint]
	// Never downgrade a routable address to loopback: behind a
	// userspace-networking tailscaled, inbound registers all appear to come
	// from 127.0.0.1 — recording that would make us "reply" to ourselves.
	if existed && isLoopback(ip) && !isLoopback(prev.IP) {
		peer.IP = prev.IP
	}
	changed := !existed || prev.IP != peer.IP || prev.Info.Alias != info.Alias
	d.peers[info.Fingerprint] = peer
	d.mu.Unlock()

	if changed {
		d.emit(Event{Kind: PeerFound, Peer: peer})
	}
}

func (d *Discoverer) emit(ev Event) {
	select {
	case d.events <- ev:
	default: // drop rather than block the network goroutine
	}
}
