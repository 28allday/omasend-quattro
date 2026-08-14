package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"omasend/internal/protocol"
)

// TestReceiveFlow drives the full receiver path: prepare-upload (auto-accepted)
// then upload, and asserts the bytes land intact in the receive dir.
func TestReceiveFlow(t *testing.T) {
	dir := t.TempDir()
	info := protocol.DeviceInfo{Alias: "recv", Version: protocol.ProtocolVersion, Port: 53996, Protocol: "http"}
	s := New(Options{Info: info, ReceiveDir: dir, AutoAccept: true})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Drain transfer events so the buffered channel never matters.
	go func() {
		for range s.Transfers() {
		}
	}()
	time.Sleep(50 * time.Millisecond)

	base := "http://127.0.0.1:53996"
	payload := []byte("hello localsend, this is the file body")

	// prepare-upload
	prep := protocol.PrepareUploadRequest{
		Info: protocol.DeviceInfo{Alias: "sender", Fingerprint: "send1234", Version: "2.1"},
		Files: map[string]protocol.FileMetadata{
			"f1": {ID: "f1", FileName: "note.txt", Size: int64(len(payload)), FileType: "text/plain"},
		},
	}
	body, _ := json.Marshal(prep)
	resp, err := http.Post(base+protocol.PathPrepareUpload, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("prepare-upload: %v", err)
	}
	var pr protocol.PrepareUploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		t.Fatalf("decode prepare response: %v", err)
	}
	resp.Body.Close()
	token, ok := pr.Files["f1"]
	if !ok || pr.SessionID == "" {
		t.Fatalf("missing session/token: %+v", pr)
	}

	// upload
	url := base + protocol.PathUpload + "?sessionId=" + pr.SessionID + "&fileId=f1&token=" + token
	up, err := http.Post(url, "application/octet-stream", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if up.StatusCode != http.StatusOK {
		t.Fatalf("upload status = %d", up.StatusCode)
	}
	up.Body.Close()

	// verify file contents
	got, err := os.ReadFile(filepath.Join(dir, "note.txt"))
	if err != nil {
		t.Fatalf("read received file: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("content mismatch: got %q", got)
	}

	// a bad token must be rejected
	bad, _ := http.Post(base+protocol.PathUpload+"?sessionId="+pr.SessionID+"&fileId=f1&token=wrong",
		"application/octet-stream", bytes.NewReader(payload))
	if bad.StatusCode != http.StatusForbidden {
		t.Fatalf("bad token status = %d, want 403", bad.StatusCode)
	}
	bad.Body.Close()
}
