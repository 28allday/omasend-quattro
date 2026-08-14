// Package app bridges the Tea-agnostic domain layer (discovery, server, client)
// into Bubble Tea messages. Domain components emit on Go channels; a bridge
// goroutine forwards them via tea.Program.Send.
package app

import (
	"context"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"omasend/internal/discovery"
	"omasend/internal/server"
	"omasend/internal/transfer"
)

// PeerFoundMsg is delivered when a peer is discovered or its address changes.
type PeerFoundMsg struct{ Peer discovery.Peer }

// PeerLostMsg is delivered when a peer ages out (post-M1).
type PeerLostMsg struct{ Fingerprint string }

// IncomingMsg is delivered when a peer asks to send us files. The TUI shows an
// accept prompt and answers via the carried Reply channel.
type IncomingMsg struct{ Req server.AcceptRequest }

// TransferMsg reports progress/lifecycle of a transfer (either direction).
type TransferMsg struct{ Ev transfer.Event }

// MessageMsg is delivered when a peer sends us a plain-text message.
type MessageMsg struct{ Msg server.ReceivedMessage }

// BridgeServer forwards the server's accept requests, transfer events, and
// received messages to the Tea program until ctx is cancelled. notify, if
// non-nil, is called (summary, body) for events worth a desktop notification —
// an inbound message or a peer offering files — so a backgrounded receiver
// surfaces them through the desktop's notification daemon.
func BridgeServer(ctx context.Context, accepts <-chan server.AcceptRequest, transfers <-chan transfer.Event, messages <-chan server.ReceivedMessage, send func(tea.Msg), notify func(summary, body string)) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case req := <-accepts:
				if notify != nil {
					notify(notifyTitleFrom(req.From.Alias)+" wants to send files", incomingFilesBody(req))
				}
				send(IncomingMsg{Req: req})
			case ev := <-transfers:
				send(TransferMsg{Ev: ev})
			case m := <-messages:
				if notify != nil {
					notify("Message from "+nonEmptyAlias(m.From), m.Text)
				}
				send(MessageMsg{Msg: m})
			}
		}
	}()
}

// nonEmptyAlias returns alias, or a generic stand-in when a peer sent no name.
func nonEmptyAlias(alias string) string {
	if alias == "" {
		return "a device"
	}
	return alias
}

func notifyTitleFrom(alias string) string { return nonEmptyAlias(alias) }

// incomingFilesBody summarises an incoming file offer for a notification body,
// naming the file when there's only one and counting them otherwise.
func incomingFilesBody(req server.AcceptRequest) string {
	switch len(req.Files) {
	case 0:
		return "Incoming transfer"
	case 1:
		for _, f := range req.Files {
			return f.FileName
		}
	}
	return fmt.Sprintf("%d files", len(req.Files))
}

// BridgeTransfers forwards a transfer event channel (e.g. the sender's) to the
// Tea program until ctx is cancelled.
func BridgeTransfers(ctx context.Context, transfers <-chan transfer.Event, send func(tea.Msg)) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-transfers:
				send(TransferMsg{Ev: ev})
			}
		}
	}()
}

// BridgeDiscovery forwards discovery events to the Tea program until ctx is
// cancelled. send is typically tea.Program.Send.
func BridgeDiscovery(ctx context.Context, events <-chan discovery.Event, send func(tea.Msg)) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-events:
				switch ev.Kind {
				case discovery.PeerFound:
					send(PeerFoundMsg{Peer: ev.Peer})
				case discovery.PeerLost:
					send(PeerLostMsg{Fingerprint: ev.Peer.Info.Fingerprint})
				}
			}
		}
	}()
}
