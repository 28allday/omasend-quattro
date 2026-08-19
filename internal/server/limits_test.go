package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// TestDanglingSymlinkIsNotFollowed is the traversal guard at the file level: a
// dangling symlink sitting at the destination name used to read as "free"
// (os.Stat follows it), so the write went through the link to wherever it
// pointed. The target must be left untouched.
func TestDanglingSymlinkIsNotFollowed(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "victim.txt")

	// A link named as the file we are about to receive, pointing outside.
	if err := os.Symlink(outside, filepath.Join(dir, "note.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	got, err := destPath(dir, "note.txt")
	if err != nil {
		t.Fatalf("destPath: %v", err)
	}
	if got == filepath.Join(dir, "note.txt") {
		t.Fatalf("destPath handed back the symlink path %q", got)
	}
	if _, err := os.Lstat(outside); !os.IsNotExist(err) {
		t.Fatalf("link target was created at %q", outside)
	}
}

// TestSymlinkedSubdirIsRejected covers the folder-send case the lexical check
// cannot see: "sub/file.txt" where sub is a link out of the receive dir.
func TestSymlinkedSubdirIsRejected(t *testing.T) {
	dir := t.TempDir()
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(dir, "sub")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := destPath(dir, "sub/file.txt"); err == nil {
		t.Fatalf("destPath accepted a path crossing a symlinked directory")
	}

	// The ordinary nested case must still work.
	if _, err := destPath(dir, "real/file.txt"); err != nil {
		t.Fatalf("destPath rejected a legitimate nested path: %v", err)
	}
}

// TestWriteRefusesExistingSymlink proves the create itself is O_NOFOLLOW: even
// if a link were planted between the destPath call and the open, the write
// fails rather than following it.
func TestWriteRefusesExistingSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.Symlink(outside, filepath.Join(dir, "planted.txt.part")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	s := New(Options{Info: protocol.DeviceInfo{Alias: "recv"}, ReceiveDir: dir})
	go func() {
		for range s.Transfers() {
		}
	}()
	sess, _, _ := s.sessions.create(protocol.DeviceInfo{Alias: "peer"}, "127.0.0.1",
		map[string]protocol.FileMetadata{"f1": {ID: "f1", FileName: "planted.txt", Size: 4}})
	fe := &fileEntry{meta: protocol.FileMetadata{ID: "f1", FileName: "planted.txt", Size: 4}}

	if _, err := s.writeFile(sess, fe, "k", strings.NewReader("data")); err == nil {
		if _, statErr := os.Lstat(outside); !os.IsNotExist(statErr) {
			t.Fatalf("wrote through the planted symlink to %q", outside)
		}
	}
	if _, err := os.Lstat(outside); !os.IsNotExist(err) {
		t.Fatalf("link target was created at %q", outside)
	}
}

// TestSessionsAreBounded is the auto-accept case the review raised: a peer that
// calls prepare-upload over and over and never uploads must not be able to
// accumulate metadata maps without limit.
func TestSessionsAreBounded(t *testing.T) {
	store := newSessionStore()
	files := map[string]protocol.FileMetadata{
		"f1": {ID: "f1", FileName: "a.bin", Size: 1},
	}
	peer := protocol.DeviceInfo{Alias: "greedy", Fingerprint: "greedy00"}

	for i := 0; i < maxSessions; i++ {
		if _, _, err := store.create(peer, "127.0.0.1", files); err != nil {
			t.Fatalf("session %d refused early: %v", i, err)
		}
	}
	if _, _, err := store.create(peer, "127.0.0.1", files); !errors.Is(err, ErrTooManySessions) {
		t.Fatalf("session past the cap was accepted (err=%v)", err)
	}
}

// TestIdleSessionsExpire proves the cap cannot be reached by abandoned
// sessions alone — they age out and the slot comes back.
func TestIdleSessionsExpire(t *testing.T) {
	store := newSessionStore()
	files := map[string]protocol.FileMetadata{"f1": {ID: "f1", FileName: "a.bin", Size: 1}}
	peer := protocol.DeviceInfo{Alias: "quiet", Fingerprint: "quiet000"}

	sess, _, err := store.create(peer, "127.0.0.1", files)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Backdate it past the TTL, as an abandoned session would be.
	store.mu.Lock()
	store.sessions[sess.id].lastUsed = time.Now().Add(-2 * sessionTTL)
	store.mu.Unlock()

	store.sweep()

	store.mu.Lock()
	n := len(store.sessions)
	store.mu.Unlock()
	if n != 0 {
		t.Fatalf("idle session survived the sweep (%d left)", n)
	}
}

// TestBusySessionSurvivesSweep is the guard on the guard: a long upload must
// never have its session expired out from under it.
func TestBusySessionSurvivesSweep(t *testing.T) {
	store := newSessionStore()
	files := map[string]protocol.FileMetadata{"f1": {ID: "f1", FileName: "big.bin", Size: 1}}
	sess, _, err := store.create(protocol.DeviceInfo{Alias: "slow"}, "127.0.0.1", files)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	store.beginUpload(sess.id)
	store.mu.Lock()
	store.sessions[sess.id].lastUsed = time.Now().Add(-10 * sessionTTL)
	store.mu.Unlock()

	store.sweep()

	store.mu.Lock()
	_, alive := store.sessions[sess.id]
	store.mu.Unlock()
	if !alive {
		t.Fatalf("a session with an upload in flight was expired")
	}

	// Once the upload finishes it becomes eligible again.
	store.endUpload(sess.id)
	store.mu.Lock()
	store.sessions[sess.id].lastUsed = time.Now().Add(-10 * sessionTTL)
	store.mu.Unlock()
	store.sweep()
	store.mu.Lock()
	_, stillAlive := store.sessions[sess.id]
	store.mu.Unlock()
	if stillAlive {
		t.Fatalf("session survived the sweep after its upload ended")
	}
}
