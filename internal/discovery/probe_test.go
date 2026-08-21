package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"omasend/internal/protocol"
)

// TestProbeRegistersPeer drives Probe against a stub /register that behaves like
// a real peer: it records the caller and returns its own DeviceInfo. The https
// attempt fails against the plain-http test server and Probe falls back to http.
func TestProbeRegistersPeer(t *testing.T) {
	peerInfo := protocol.DeviceInfo{Alias: "Remote", Fingerprint: "remote-fp", Port: 53317, Protocol: "http"}
	var sawOurInfo bool

	mux := http.NewServeMux()
	mux.HandleFunc("/api/localsend/v2/register", func(w http.ResponseWriter, r *http.Request) {
		var in protocol.DeviceInfo
		if err := json.NewDecoder(r.Body).Decode(&in); err == nil && in.Fingerprint == "self-fp" {
			sawOurInfo = true
		}
		_ = json.NewEncoder(w).Encode(peerInfo)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://") // host:port

	d := New(protocol.DeviceInfo{Fingerprint: "self-fp", Alias: "Self"})
	if err := d.Probe(context.Background(), host); err != nil {
		t.Fatalf("Probe failed: %v", err)
	}
	if !sawOurInfo {
		t.Error("peer did not receive our device info in the register body")
	}
	peers := d.Snapshot()
	if len(peers) != 1 || peers[0].Info.Fingerprint != "remote-fp" {
		t.Fatalf("peer not recorded as expected: %+v", peers)
	}
	if wantIP := strings.Split(host, ":")[0]; peers[0].IP != wantIP {
		t.Errorf("peer IP = %q, want %q (the host we dialed)", peers[0].IP, wantIP)
	}
}

// TestFindPeerViaProbe covers the headless-send path for remote peers: a peer
// that multicast can't see (here: only reachable by unicast Probe) must still
// satisfy a FindPeer that is already waiting — Probe → NotePeer → PeerFound.
func TestFindPeerViaProbe(t *testing.T) {
	peerInfo := protocol.DeviceInfo{Alias: "titan-box", Fingerprint: "titan-fp", Port: 53317, Protocol: "http"}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/localsend/v2/register", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(peerInfo)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	d := New(protocol.DeviceInfo{Fingerprint: "self-fp", Alias: "Self"})

	// Probe concurrently, like watchRemotes does while FindPeer waits.
	go func() {
		if err := d.Probe(context.Background(), host); err != nil {
			t.Errorf("Probe failed: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := d.FindPeer(ctx, func(p Peer) bool {
		return strings.EqualFold(strings.TrimSpace(p.Info.Alias), "titan-box")
	})
	if err != nil {
		t.Fatalf("FindPeer did not see the probed peer: %v", err)
	}
	if got.Info.Fingerprint != "titan-fp" {
		t.Errorf("fingerprint = %q, want titan-fp", got.Info.Fingerprint)
	}
	if wantIP := strings.Split(host, ":")[0]; got.IP != wantIP {
		t.Errorf("peer IP = %q, want %q (the host probed)", got.IP, wantIP)
	}
}

func TestProbeUnreachableErrors(t *testing.T) {
	d := New(protocol.DeviceInfo{Fingerprint: "self-fp"})
	// 127.0.0.1:1 — nothing listening; both https and http should fail fast.
	if err := d.Probe(context.Background(), "127.0.0.1:1"); err == nil {
		t.Fatal("expected an error probing an unreachable host")
	}
}

// A register arriving via a userspace-networking tailscaled proxy appears to
// come from 127.0.0.1; that must not overwrite a peer's known routable address
// (sending to it would loop back to our own receiver).
func TestNotePeerKeepsRoutableOverLoopback(t *testing.T) {
	d := New(protocol.DeviceInfo{Fingerprint: "self-fp"})
	info := protocol.DeviceInfo{Alias: "gav", Fingerprint: "gav-fp", Port: 53317}

	d.NotePeer(info, "100.91.41.111")
	d.NotePeer(info, "127.0.0.1") // inbound register through the local proxy
	if got := d.Snapshot()[0].IP; got != "100.91.41.111" {
		t.Errorf("IP downgraded to %q, want 100.91.41.111 kept", got)
	}

	// A first sight at loopback is still recorded (nothing better known)…
	d2 := New(protocol.DeviceInfo{Fingerprint: "self-fp"})
	d2.NotePeer(info, "127.0.0.1")
	if got := d2.Snapshot()[0].IP; got != "127.0.0.1" {
		t.Errorf("first-sight IP = %q, want 127.0.0.1", got)
	}
	// …and upgrades to the routable address as soon as one is learned.
	d2.NotePeer(info, "100.91.41.111")
	if got := d2.Snapshot()[0].IP; got != "100.91.41.111" {
		t.Errorf("IP = %q, want upgrade to 100.91.41.111", got)
	}
}

func TestHostPortDefaults(t *testing.T) {
	if h, p := hostPort("colossus"); h != "colossus" || p != protocol.DefaultPort {
		t.Errorf("hostPort(bare) = %q,%d; want colossus,%d", h, p, protocol.DefaultPort)
	}
	if h, p := hostPort("100.64.0.2:9999"); h != "100.64.0.2" || p != 9999 {
		t.Errorf("hostPort(host:port) = %q,%d; want 100.64.0.2,9999", h, p)
	}
}

// The peer table is capped: announces are unauthenticated, so unknown
// fingerprints past maxPeers are dropped — and a known peer still updates,
// so a full table cannot push real devices out.
func TestNotePeerBoundsTable(t *testing.T) {
	d := New(protocol.DeviceInfo{Fingerprint: "self-fp"})
	for i := 0; i < maxPeers; i++ {
		d.NotePeer(protocol.DeviceInfo{Alias: "fake", Fingerprint: fmt.Sprintf("fp-%d", i), Port: 1}, "10.0.0.1")
	}
	d.NotePeer(protocol.DeviceInfo{Alias: "late", Fingerprint: "fp-late", Port: 1}, "10.0.0.2")
	if got := len(d.Snapshot()); got != maxPeers {
		t.Fatalf("peer table = %d entries, want capped at %d", got, maxPeers)
	}
	// A peer already in the table still updates at the cap.
	d.NotePeer(protocol.DeviceInfo{Alias: "renamed", Fingerprint: "fp-0", Port: 1}, "10.0.0.3")
	for _, p := range d.Snapshot() {
		if p.Info.Fingerprint == "fp-0" && p.Info.Alias != "renamed" {
			t.Fatal("known peer failed to update at the cap")
		}
	}
}
