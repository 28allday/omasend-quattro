package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"omasend/internal/protocol"
)

func TestInfoEndpoint(t *testing.T) {
	info := protocol.DeviceInfo{
		Alias:       "test-host",
		Version:     protocol.ProtocolVersion,
		DeviceType:  protocol.DeviceServer,
		Fingerprint: "deadbeef",
		Port:        53999, // off the default to avoid clashing with a running instance
		Protocol:    "http",
	}
	s := New(Options{Info: info})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Give Serve a beat to accept connections.
	time.Sleep(50 * time.Millisecond)

	resp, err := http.Get("http://127.0.0.1:53999" + protocol.PathInfo)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got protocol.DeviceInfo
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Alias != "test-host" || got.Version != "2.1" || got.Fingerprint != "deadbeef" {
		t.Fatalf("unexpected info: %+v", got)
	}
	if got.Protocol != "http" {
		t.Fatalf("protocol = %q, want http", got.Protocol)
	}
}
