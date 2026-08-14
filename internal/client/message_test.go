package client

import (
	"context"
	"os"
	"testing"
	"time"

	"omasend/internal/discovery"
	"omasend/internal/protocol"
	"omasend/internal/server"
)

// A sent message must arrive on the receiver's Messages channel with its text
// and sender intact, and must NOT be written to disk as a file.
func TestSendMessageEndToEnd(t *testing.T) {
	recvDir := t.TempDir()
	recvInfo := protocol.DeviceInfo{
		Alias: "recv", Version: protocol.ProtocolVersion, Port: 53990, Protocol: "http",
	}
	// AutoAccept is false on purpose: messages must bypass the accept prompt.
	srv := server.New(server.Options{Info: recvInfo, ReceiveDir: recvDir, AutoAccept: false})
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

	sender := New(protocol.DeviceInfo{Alias: "sender", Fingerprint: "snd1", Version: "2.1", Protocol: "http"})
	peer := discovery.Peer{Info: recvInfo, IP: "127.0.0.1"}
	sender.SendMessage(peer, "hello over the wire\nsecond line", "")

	select {
	case m := <-srv.Messages():
		if m.Text != "hello over the wire\nsecond line" {
			t.Fatalf("message text = %q", m.Text)
		}
		if m.From != "sender" {
			t.Errorf("message from = %q, want \"sender\"", m.From)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the message")
	}

	// A message is not a file: nothing should land in the receive dir.
	if entries, _ := os.ReadDir(recvDir); len(entries) != 0 {
		t.Fatalf("message should not be saved; found %d entries in receive dir", len(entries))
	}
}
