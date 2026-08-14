package app

import (
	"context"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"omasend/internal/protocol"
	"omasend/internal/server"
	"omasend/internal/transfer"
)

// An inbound message must drive the notify callback with the sender's name and
// the message text, so a backgrounded receiver surfaces it on the desktop.
func TestBridgeServerNotifiesOnMessage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	accepts := make(chan server.AcceptRequest)
	transfers := make(chan transfer.Event)
	messages := make(chan server.ReceivedMessage, 1)

	type call struct{ summary, body string }
	got := make(chan call, 1)
	notify := func(summary, body string) { got <- call{summary, body} }

	BridgeServer(ctx, accepts, transfers, messages, func(tea.Msg) {}, notify)

	messages <- server.ReceivedMessage{From: "Strong Onion", Text: "dinner's ready"}

	select {
	case c := <-got:
		if c.summary != "Message from Strong Onion" {
			t.Errorf("summary = %q", c.summary)
		}
		if c.body != "dinner's ready" {
			t.Errorf("body = %q", c.body)
		}
	case <-time.After(time.Second):
		t.Fatal("notify was not called for an inbound message")
	}
}

// incomingFilesBody names a single file and counts multiple ones.
func TestIncomingFilesBody(t *testing.T) {
	one := server.AcceptRequest{Files: map[string]protocol.FileMetadata{
		"a": {FileName: "photo.jpg"},
	}}
	if got := incomingFilesBody(one); got != "photo.jpg" {
		t.Errorf("single file body = %q, want photo.jpg", got)
	}

	many := server.AcceptRequest{Files: map[string]protocol.FileMetadata{
		"a": {FileName: "x"}, "b": {FileName: "y"}, "c": {FileName: "z"},
	}}
	if got := incomingFilesBody(many); got != "3 files" {
		t.Errorf("multi file body = %q, want \"3 files\"", got)
	}
}

func TestNonEmptyAlias(t *testing.T) {
	if got := nonEmptyAlias(""); got != "a device" {
		t.Errorf("empty alias = %q", got)
	}
	if got := nonEmptyAlias("Slate Starburst"); got != "Slate Starburst" {
		t.Errorf("alias = %q", got)
	}
}
