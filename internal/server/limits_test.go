package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"omasend/internal/protocol"
)

// startTestServer boots a receiver on port with a drained event channel and
// returns its base URL and receive dir.
func startTestServer(t *testing.T, port int, autoAccept bool) (string, string) {
	t.Helper()
	dir := t.TempDir()
	info := protocol.DeviceInfo{Alias: "recv", Version: protocol.ProtocolVersion, Port: port, Protocol: "http"}
	s := New(Options{Info: info, ReceiveDir: dir, AutoAccept: autoAccept})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	go func() {
		for range s.Transfers() {
		}
	}()
	go func() {
		for range s.Messages() {
		}
	}()
	time.Sleep(50 * time.Millisecond)
	return "http://127.0.0.1:" + strconv.Itoa(port), dir
}

// TestPrepareUploadBodyIsCapped proves an unauthenticated peer cannot stream an
// unbounded JSON body into memory: past maxPrepareBody the decode fails and the
// handler answers 400 rather than reading on.
func TestPrepareUploadBodyIsCapped(t *testing.T) {
	base, _ := startTestServer(t, 53981, true)

	// A single file entry whose name alone overruns the cap.
	prep := protocol.PrepareUploadRequest{
		Info: protocol.DeviceInfo{Alias: "flood", Fingerprint: "flood123", Version: "2.1"},
		Files: map[string]protocol.FileMetadata{
			"f1": {ID: "f1", FileName: strings.Repeat("A", maxPrepareBody+1024), Size: 1},
		},
	}
	body, _ := json.Marshal(prep)
	if len(body) <= maxPrepareBody {
		t.Fatalf("test payload %d bytes did not exceed cap %d", len(body), maxPrepareBody)
	}

	resp, err := http.Post(base+protocol.PathPrepareUpload, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("prepare-upload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an over-long body", resp.StatusCode)
	}
}

// TestRegisterBodyIsCapped covers the other pre-authentication endpoint. The
// handler always answers with our own info, so the assertion is that an
// over-long body is not treated as a valid peer registration.
func TestRegisterBodyIsCapped(t *testing.T) {
	dir := t.TempDir()
	seen := make(chan protocol.DeviceInfo, 1)
	info := protocol.DeviceInfo{Alias: "recv", Version: protocol.ProtocolVersion, Port: 53982, Protocol: "http"}
	s := New(Options{
		Info: info, ReceiveDir: dir,
		OnPeer: func(i protocol.DeviceInfo, _ string) {
			select {
			case seen <- i:
			default:
			}
		},
	})
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

	peer := protocol.DeviceInfo{
		Alias:       strings.Repeat("B", maxRegisterBody+1024),
		Fingerprint: "peer1234",
		Version:     "2.1",
	}
	body, _ := json.Marshal(peer)
	resp, err := http.Post("http://127.0.0.1:53982"+protocol.PathRegister, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	resp.Body.Close()

	select {
	case got := <-seen:
		t.Fatalf("over-long register body was accepted as peer %q", got.Alias)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestUploadCannotExceedDeclaredSize is the disk-exhaustion guard: a sender
// declares a small file, then streams far more. The receiver must refuse and
// leave nothing behind — no completed file, no stray .part.
func TestUploadCannotExceedDeclaredSize(t *testing.T) {
	base, dir := startTestServer(t, 53983, true)

	const declared = 16
	prep := protocol.PrepareUploadRequest{
		Info: protocol.DeviceInfo{Alias: "liar", Fingerprint: "liar1234", Version: "2.1"},
		Files: map[string]protocol.FileMetadata{
			"f1": {ID: "f1", FileName: "small.bin", Size: declared, FileType: "application/octet-stream"},
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
	token := pr.Files["f1"]

	// Stream a megabyte against a 16-byte declaration.
	oversized := bytes.Repeat([]byte("x"), 1<<20)
	url := base + protocol.PathUpload + "?sessionId=" + pr.SessionID + "&fileId=f1&token=" + token
	up, err := http.Post(url, "application/octet-stream", bytes.NewReader(oversized))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	up.Body.Close()
	if up.StatusCode == http.StatusOK {
		t.Fatalf("oversized upload was accepted (status 200)")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read receive dir: %v", err)
	}
	for _, e := range entries {
		t.Fatalf("receive dir should be empty, found %q", e.Name())
	}
	if _, err := os.Stat(filepath.Join(dir, "small.bin.part")); !os.IsNotExist(err) {
		t.Fatalf("partial file was left behind")
	}
}

// TestUploadAcceptsExactDeclaredSize guards the off-by-one in the limit: a file
// of exactly the declared size must still land intact.
func TestUploadAcceptsExactDeclaredSize(t *testing.T) {
	base, dir := startTestServer(t, 53984, true)

	payload := []byte("exactly this many bytes")
	prep := protocol.PrepareUploadRequest{
		Info: protocol.DeviceInfo{Alias: "honest", Fingerprint: "hon12345", Version: "2.1"},
		Files: map[string]protocol.FileMetadata{
			"f1": {ID: "f1", FileName: "exact.txt", Size: int64(len(payload)), FileType: "application/octet-stream"},
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

	url := base + protocol.PathUpload + "?sessionId=" + pr.SessionID + "&fileId=f1&token=" + pr.Files["f1"]
	up, err := http.Post(url, "application/octet-stream", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	up.Body.Close()
	if up.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", up.StatusCode)
	}
	got, err := os.ReadFile(filepath.Join(dir, "exact.txt"))
	if err != nil {
		t.Fatalf("read received file: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("content mismatch: got %q", got)
	}
}

// TestOverlongMessageIsNotEmitted keeps a peer from parking a large blob in the
// message channel: past maxMessageText it stops being treated as a message.
func TestOverlongMessageIsNotEmitted(t *testing.T) {
	files := map[string]protocol.FileMetadata{
		"m1": {ID: "m1", FileName: "msg.txt", FileType: "text/plain", Preview: strings.Repeat("z", maxMessageText+1)},
	}
	if _, ok := messageOf(files); ok {
		t.Fatalf("over-long preview was accepted as a message")
	}

	files["m1"] = protocol.FileMetadata{ID: "m1", FileName: "msg.txt", FileType: "text/plain", Preview: "still a message"}
	text, ok := messageOf(files)
	if !ok || text != "still a message" {
		t.Fatalf("normal message rejected: %q ok=%v", text, ok)
	}
}
