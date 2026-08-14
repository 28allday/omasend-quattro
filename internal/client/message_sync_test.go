package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"omasend/internal/discovery"
	"omasend/internal/protocol"
	"omasend/internal/server"
	"omasend/internal/transfer"
)

// SendMessageSync delivers the message and returns nil, with the text arriving
// on the receiver's Messages channel — the synchronous path used by headless
// `-to/-message` sends.
func TestSendMessageSyncSuccess(t *testing.T) {
	recvInfo := protocol.DeviceInfo{
		Alias: "recv", Version: protocol.ProtocolVersion, Port: 53991, Protocol: "http",
	}
	srv := server.New(server.Options{Info: recvInfo, ReceiveDir: t.TempDir(), AutoAccept: false})
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

	sender := New(protocol.DeviceInfo{Alias: "cli", Fingerprint: "cli1", Version: "2.1", Protocol: "http"})
	peer := discovery.Peer{Info: recvInfo, IP: "127.0.0.1"}
	if err := sender.SendMessageSync(peer, "headless hello", ""); err != nil {
		t.Fatalf("SendMessageSync: %v", err)
	}

	select {
	case m := <-srv.Messages():
		if m.Text != "headless hello" {
			t.Fatalf("message text = %q", m.Text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the message")
	}
}

// A peer that requires a PIN rejects a PIN-less send with ErrPinRequired, so the
// CLI can tell the user to pass -send-pin.
func TestSendMessageSyncPinRequired(t *testing.T) {
	recvInfo := protocol.DeviceInfo{
		Alias: "recv", Version: protocol.ProtocolVersion, Port: 53992, Protocol: "http",
	}
	srv := server.New(server.Options{Info: recvInfo, ReceiveDir: t.TempDir(), PIN: "2468"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("server start: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	sender := New(protocol.DeviceInfo{Alias: "cli", Fingerprint: "cli1", Version: "2.1", Protocol: "http"})
	peer := discovery.Peer{Info: recvInfo, IP: "127.0.0.1"}
	err := sender.SendMessageSync(peer, "no pin", "")
	if !errors.Is(err, transfer.ErrPinRequired) {
		t.Fatalf("err = %v, want ErrPinRequired", err)
	}
	// With the correct PIN it goes through.
	if err := sender.SendMessageSync(peer, "with pin", "2468"); err != nil {
		t.Fatalf("SendMessageSync with PIN: %v", err)
	}
}
