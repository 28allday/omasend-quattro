package client

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"omasend/internal/discovery"
	"omasend/internal/protocol"
	"omasend/internal/server"
	"omasend/internal/transfer"
)

// TestSendToReceiver drives our sender against our receiver over loopback HTTP
// and asserts the file arrives intact — the M3 end-to-end guarantee.
func TestSendToReceiver(t *testing.T) {
	recvDir := t.TempDir()
	recvInfo := protocol.DeviceInfo{
		Alias: "recv", Version: protocol.ProtocolVersion, Port: 53995, Protocol: "http",
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

	// Source file to send.
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "photo.jpg")
	content := bytes.Repeat([]byte("localsend-payload-"), 5000) // ~90KB
	if err := os.WriteFile(srcPath, content, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	sender := New(protocol.DeviceInfo{Alias: "sender", Fingerprint: "snd1", Version: "2.1", Protocol: "http"})
	peer := discovery.Peer{Info: recvInfo, IP: "127.0.0.1"}
	sender.Send(peer, []string{srcPath}, "")

	// Wait for the outgoing FileDone (or fail on error/timeout).
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-sender.Events():
			if ev.Kind == transfer.Error {
				t.Fatalf("send error: %v", ev.Err)
			}
			if ev.Kind == transfer.FileDone {
				goto verify
			}
		case <-deadline:
			t.Fatal("timed out waiting for send to complete")
		}
	}

verify:
	got, err := os.ReadFile(filepath.Join(recvDir, "photo.jpg"))
	if err != nil {
		t.Fatalf("read received: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("content mismatch: %d vs %d bytes", len(got), len(content))
	}
}
