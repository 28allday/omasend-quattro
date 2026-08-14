package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"omasend/internal/protocol"
	"omasend/internal/security"
)

// TestTLSFingerprintMatches is the core HTTPS-interop guarantee: the
// fingerprint we advertise must equal the SHA-256 of the certificate our TLS
// server actually presents, so a peer that pins the announced fingerprint
// validates us.
func TestTLSFingerprintMatches(t *testing.T) {
	id, err := security.Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	cert, err := tls.X509KeyPair([]byte(id.CertPEM), []byte(id.KeyPEM))
	if err != nil {
		t.Fatalf("keypair: %v", err)
	}

	info := protocol.DeviceInfo{
		Alias: "tls-host", Version: protocol.ProtocolVersion,
		Fingerprint: id.Fingerprint, Port: 53997, Protocol: "https",
	}
	s := New(Options{Info: info, Cert: &cert})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	resp, err := client.Get("https://127.0.0.1:53997" + protocol.PathInfo)
	if err != nil {
		t.Fatalf("https get: %v", err)
	}
	defer resp.Body.Close()

	// The cert the server presented must hash to the advertised fingerprint.
	served := resp.TLS.PeerCertificates[0].Raw
	if got := security.Fingerprint(served); got != id.Fingerprint {
		t.Fatalf("served cert fingerprint %s != advertised %s", got, id.Fingerprint)
	}

	var di protocol.DeviceInfo
	if err := json.NewDecoder(resp.Body).Decode(&di); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if di.Protocol != "https" || di.Fingerprint != id.Fingerprint {
		t.Fatalf("unexpected /info over TLS: %+v", di)
	}
}
