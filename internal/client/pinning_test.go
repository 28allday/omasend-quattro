package client

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"omasend/internal/discovery"
	"omasend/internal/protocol"
	"omasend/internal/security"
)

// peerFor builds a Peer pointing at srv but advertising the given fingerprint.
func peerFor(t *testing.T, srv *httptest.Server, fingerprint string) discovery.Peer {
	t.Helper()
	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	if err != nil {
		t.Fatalf("split %q: %v", srv.URL, err)
	}
	port, _ := strconv.Atoi(portStr)
	return discovery.Peer{
		IP: host,
		Info: protocol.DeviceInfo{
			Alias:       "peer",
			Protocol:    "https",
			Port:        port,
			Fingerprint: fingerprint,
		},
	}
}

// TestPinnedClientRejectsWrongCertificate is the on-path-attacker case: the
// device we picked advertised one fingerprint, but whatever answers presents a
// different certificate. The handshake must fail — chain validation is off by
// necessity, so the fingerprint is the only thing authenticating the peer.
func TestPinnedClientRejectsWrongCertificate(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender := New(protocol.DeviceInfo{Alias: "cli", Fingerprint: "cli1", Version: "2.1"})

	hc := sender.clientFor(strings.Repeat("A", 64)) // not this server's cert
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	if _, err := hc.Do(req); err == nil {
		t.Fatalf("pinned client accepted a certificate it should have refused")
	} else if !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("expected a fingerprint mismatch, got: %v", err)
	}
}

// TestPinnedClientAcceptsTheRealCertificate is the other half — pinning must
// not break talking to the genuine device.
func TestPinnedClientAcceptsTheRealCertificate(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	want := security.Fingerprint(srv.TLS.Certificates[0].Certificate[0])

	sender := New(protocol.DeviceInfo{Alias: "cli", Fingerprint: "cli1", Version: "2.1"})
	hc := sender.clientFor(want)
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("pinned client rejected the matching certificate: %v", err)
	}
	resp.Body.Close()
}

// TestSendMessageRefusesImpostor drives the same protection through the actual
// send path, so the pinned client is proven to be the one sends use.
func TestSendMessageRefusesImpostor(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("impostor received a request for %s — payload leaked", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender := New(protocol.DeviceInfo{Alias: "cli", Fingerprint: "cli1", Version: "2.1"})
	peer := peerFor(t, srv, strings.Repeat("A", 64))

	if err := sender.SendMessageSync(peer, "secret text", "1234"); err == nil {
		t.Fatalf("message send succeeded against a peer with the wrong fingerprint")
	}
}

// TestUnknownFingerprintStillConnects keeps the escape hatch honest: a peer we
// have no fingerprint for (plain http, or one added by hand before we have
// seen its certificate) must still be reachable.
func TestUnknownFingerprintStillConnects(t *testing.T) {
	sender := New(protocol.DeviceInfo{Alias: "cli", Fingerprint: "cli1", Version: "2.1"})
	if sender.clientFor("") != sender.http {
		t.Fatalf("an empty fingerprint should fall back to the unpinned client")
	}
}
