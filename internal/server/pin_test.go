package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"omasend/internal/protocol"
)

// TestPrepareUploadPIN verifies the PIN gate: missing/wrong PIN -> 401, correct
// PIN -> 200 with a session.
func TestPrepareUploadPIN(t *testing.T) {
	dir := t.TempDir()
	info := protocol.DeviceInfo{Alias: "recv", Version: protocol.ProtocolVersion, Port: 53994, Protocol: "http"}
	s := New(Options{Info: info, ReceiveDir: dir, AutoAccept: true, PIN: "2468"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	go func() {
		for range s.Transfers() {
		}
	}()
	time.Sleep(50 * time.Millisecond)

	base := "http://127.0.0.1:53994" + protocol.PathPrepareUpload
	body, _ := json.Marshal(protocol.PrepareUploadRequest{
		Info:  protocol.DeviceInfo{Alias: "snd", Fingerprint: "x", Version: "2.1"},
		Files: map[string]protocol.FileMetadata{"f1": {ID: "f1", FileName: "a.txt", Size: 1}},
	})

	post := func(url string) int {
		resp, err := http.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if code := post(base); code != http.StatusUnauthorized {
		t.Fatalf("no PIN: got %d, want 401", code)
	}
	if code := post(base + "?pin=0000"); code != http.StatusUnauthorized {
		t.Fatalf("wrong PIN: got %d, want 401", code)
	}
	if code := post(base + "?pin=2468"); code != http.StatusOK {
		t.Fatalf("correct PIN: got %d, want 200", code)
	}
}
