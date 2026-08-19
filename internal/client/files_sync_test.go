package client

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"omasend/internal/discovery"
	"omasend/internal/protocol"
	"omasend/internal/server"
	"omasend/internal/transfer"
)

// SendFilesSync delivers a file and a folder over loopback HTTP and returns
// nil, with the contents arriving intact — the synchronous path used by
// headless `-to <alias> <paths…>` sends.
func TestSendFilesSyncSuccess(t *testing.T) {
	recvDir := t.TempDir()
	recvInfo := protocol.DeviceInfo{
		Alias: "recv", Version: protocol.ProtocolVersion, Port: 53993, Protocol: "http",
	}
	srv := server.New(server.Options{Info: recvInfo, ReceiveDir: recvDir, AutoAccept: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("server start: %v", err)
	}
	go func() {
		for range srv.Transfers() {
		}
	}()
	time.Sleep(50 * time.Millisecond)

	// One loose file plus a folder, so the relative-name path is covered too.
	srcDir := t.TempDir()
	loose := filepath.Join(srcDir, "report.pdf")
	content := bytes.Repeat([]byte("headless-payload-"), 5000) // ~85KB
	if err := os.WriteFile(loose, content, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	folder := filepath.Join(srcDir, "Trip")
	if err := os.MkdirAll(filepath.Join(folder, "day1"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	nested := filepath.Join(folder, "day1", "img.jpg")
	if err := os.WriteFile(nested, []byte("nested"), 0o644); err != nil {
		t.Fatalf("write nested: %v", err)
	}

	sender := New(protocol.DeviceInfo{Alias: "cli", Fingerprint: "cli1", Version: "2.1", Protocol: "http"})
	peer := discovery.Peer{Info: recvInfo, IP: "127.0.0.1"}

	var done []string
	err := sender.SendFilesSync(ctx, peer, []string{loose, folder}, "", func(name string, size int64) {
		done = append(done, name)
	})
	if err != nil {
		t.Fatalf("SendFilesSync: %v", err)
	}
	if len(done) != 2 {
		t.Fatalf("onDone called %d times, want 2 (%v)", len(done), done)
	}

	got, err := os.ReadFile(filepath.Join(recvDir, "report.pdf"))
	if err != nil {
		t.Fatalf("read received: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("content mismatch: %d vs %d bytes", len(got), len(content))
	}
	if _, err := os.Stat(filepath.Join(recvDir, "Trip", "day1", "img.jpg")); err != nil {
		t.Fatalf("folder structure not recreated: %v", err)
	}
}

// A missing path is a hard error before any network work — a script passing a
// wrong path wants a non-zero exit, not a silent skip.
func TestSendFilesSyncMissingPath(t *testing.T) {
	sender := New(protocol.DeviceInfo{Alias: "cli", Fingerprint: "cli1", Version: "2.1", Protocol: "http"})
	peer := discovery.Peer{Info: protocol.DeviceInfo{Protocol: "http", Port: 1}, IP: "127.0.0.1"}
	err := sender.SendFilesSync(context.Background(), peer, []string{"/no/such/file"}, "", nil)
	if err == nil || !os.IsNotExist(errors.Unwrap(err)) && !os.IsNotExist(err) {
		t.Fatalf("err = %v, want not-exist", err)
	}
}

// A peer that requires a PIN rejects a PIN-less file send with ErrPinRequired,
// so the CLI can tell the user to pass -send-pin; with the PIN it goes through.
func TestSendFilesSyncPinRequired(t *testing.T) {
	recvDir := t.TempDir()
	recvInfo := protocol.DeviceInfo{
		Alias: "recv", Version: protocol.ProtocolVersion, Port: 53970, Protocol: "http",
	}
	srv := server.New(server.Options{Info: recvInfo, ReceiveDir: recvDir, AutoAccept: true, PIN: "2468"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("server start: %v", err)
	}
	go func() {
		for range srv.Transfers() {
		}
	}()
	time.Sleep(50 * time.Millisecond)

	src := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(src, []byte("pinned"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	sender := New(protocol.DeviceInfo{Alias: "cli", Fingerprint: "cli1", Version: "2.1", Protocol: "http"})
	peer := discovery.Peer{Info: recvInfo, IP: "127.0.0.1"}

	err := sender.SendFilesSync(ctx, peer, []string{src}, "", nil)
	if !errors.Is(err, transfer.ErrPinRequired) {
		t.Fatalf("err = %v, want ErrPinRequired", err)
	}
	if err := sender.SendFilesSync(ctx, peer, []string{src}, "2468", nil); err != nil {
		t.Fatalf("SendFilesSync with PIN: %v", err)
	}
	if _, err := os.Stat(filepath.Join(recvDir, "note.txt")); err != nil {
		t.Fatalf("file not received: %v", err)
	}
}
