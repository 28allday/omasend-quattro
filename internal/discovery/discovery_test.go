package discovery

import (
	"context"
	"testing"
	"time"

	"omasend/internal/protocol"
	"omasend/internal/server"
)

func mkInfo(alias, fp string, port int) protocol.DeviceInfo {
	return protocol.DeviceInfo{
		Alias:       alias,
		Version:     protocol.ProtocolVersion,
		DeviceType:  protocol.DeviceServer,
		Fingerprint: fp,
		Port:        port,
		Protocol:    "http",
	}
}

// TestMutualDiscovery runs two full nodes (discovery + HTTP server) on the same
// host with different HTTP ports and asserts they discover each other over
// multicast within a short window. Requires a multicast-capable loopback path.
func TestMutualDiscovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	infoA := mkInfo("node-a", "aaaa1111", 54101)
	infoB := mkInfo("node-b", "bbbb2222", 54102)

	discA := New(infoA)
	discB := New(infoB)

	srvA := server.New(server.Options{Info: infoA, OnPeer: discA.NotePeer})
	srvB := server.New(server.Options{Info: infoB, OnPeer: discB.NotePeer})
	if err := srvA.Start(ctx); err != nil {
		t.Fatalf("srvA: %v", err)
	}
	if err := srvB.Start(ctx); err != nil {
		t.Fatalf("srvB: %v", err)
	}
	if err := discA.Run(ctx); err != nil {
		t.Fatalf("discA: %v", err)
	}
	if err := discB.Run(ctx); err != nil {
		t.Fatalf("discB: %v", err)
	}

	discA.Announce()
	discB.Announce()

	if !waitForPeer(t, discA.Events(), "bbbb2222") {
		t.Fatal("node-a did not discover node-b")
	}
	if !waitForPeer(t, discB.Events(), "aaaa1111") {
		t.Fatal("node-b did not discover node-a")
	}
}

// TestReapEvictsStalePeer checks that a peer older than peerTTL is evicted and
// a fresh one is kept.
func TestReapEvictsStalePeer(t *testing.T) {
	d := New(mkInfo("self", "selffp", 1))
	now := time.Now()
	d.peers["stale"] = Peer{Info: mkInfo("old", "stale", 2), IP: "1.1.1.1", LastSeen: now.Add(-peerTTL - time.Second)}
	d.peers["fresh"] = Peer{Info: mkInfo("new", "fresh", 3), IP: "2.2.2.2", LastSeen: now}

	if n := d.reapOnce(now); n != 1 {
		t.Fatalf("evicted %d, want 1", n)
	}
	if _, ok := d.peers["stale"]; ok {
		t.Fatal("stale peer not evicted")
	}
	if _, ok := d.peers["fresh"]; !ok {
		t.Fatal("fresh peer wrongly evicted")
	}
	select {
	case ev := <-d.Events():
		if ev.Kind != PeerLost || ev.Peer.Info.Fingerprint != "stale" {
			t.Fatalf("unexpected event: %+v", ev)
		}
	default:
		t.Fatal("expected a PeerLost event")
	}
}

func waitForPeer(t *testing.T, events <-chan Event, fingerprint string) bool {
	t.Helper()
	deadline := time.After(8 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Kind == PeerFound && ev.Peer.Info.Fingerprint == fingerprint {
				return true
			}
		case <-deadline:
			return false
		}
	}
}
