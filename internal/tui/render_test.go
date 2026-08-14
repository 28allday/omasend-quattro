package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"omasend/internal/app"
	"omasend/internal/config"
	"omasend/internal/discovery"
	"omasend/internal/protocol"
	"omasend/internal/transfer"
)

func testModel(t *testing.T) Model {
	t.Helper()
	cfg := config.Config{
		Alias: "omarchy", Port: 53317, ReceiveDir: "~/Omarchy-Send",
		DeviceType: "server", Protocol: "https",
		Fingerprint: "E24D7564CC4FFAB7337FB68BC1CE6284F524D1CEAC8FA24FEDB8130BCD2068AB",
	}
	m := New(cfg, nil)
	m.ips = []string{"192.168.1.97"}
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return nm.(Model)
}

func feedPeer(m Model, alias, ip string, dt protocol.DeviceType) Model {
	peer := discovery.Peer{Info: protocol.DeviceInfo{Alias: alias, DeviceType: dt, Port: 53317, Fingerprint: alias}, IP: ip}
	nm, _ := m.Update(app.PeerFoundMsg{Peer: peer})
	return nm.(Model)
}

// TestRenderDump prints each screen at 100x30 for visual inspection (go test
// -run RenderDump -v) and asserts the frame chrome is present.
func TestRenderDump(t *testing.T) {
	m := testModel(t)
	m = feedPeer(m, "iPhone", "192.168.1.34", protocol.DeviceMobile)
	m = feedPeer(m, "MacBook", "192.168.1.43", protocol.DeviceDesktop)

	out := m.View()
	for _, want := range []string{"Omarchy-Send", "Devices", "Transfers", "Settings", "192.168.1.97"} {
		if !strings.Contains(out, want) {
			t.Errorf("Devices view missing %q", want)
		}
	}
	fmt.Println("\n===================== PEERS =====================")
	fmt.Println(out)

	mt, _ := m.Update(app.TransferMsg{Ev: transfer.Event{Dir: transfer.Incoming, Kind: transfer.Progress, ID: "s:1", FileName: "IMG_0480.DNG", Received: 1_200_000, Total: 4_455_339}})
	m = mt.(Model)
	mt, _ = m.Update(app.TransferMsg{Ev: transfer.Event{Dir: transfer.Outgoing, Kind: transfer.FileDone, ID: "s:2", FileName: "holiday.mp4", Received: 8_000_000, Total: 8_000_000}})
	m = mt.(Model)
	m.screen = screenTransfers
	fmt.Println("\n=================== TRANSFERS ===================")
	fmt.Println(m.View())

	m.screen = screenSettings
	fmt.Println("\n=================== SETTINGS ====================")
	fmt.Println(m.View())
}
