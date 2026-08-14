package tui

import (
	"fmt"
	"io"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"omasend/internal/discovery"
	"omasend/internal/protocol"
)

// peerItem adapts a discovered peer to bubbles/list.Item.
type peerItem struct{ p discovery.Peer }

func (i peerItem) Title() string       { return i.p.Info.Alias }
func (i peerItem) Description() string { return string(i.p.Info.DeviceType) }
func (i peerItem) FilterValue() string { return i.p.Info.Alias }

// deviceIcon returns a Nerd Font glyph for a device type (the Omarchy terminal
// ships a Nerd Font; degrades to a box glyph elsewhere, which is cosmetic only).
func deviceIcon(dt protocol.DeviceType) string {
	switch dt {
	case protocol.DeviceMobile:
		return "" // phone
	case protocol.DeviceDesktop:
		return "" // desktop
	case protocol.DeviceServer, protocol.DeviceHeadless:
		return "" // server
	case protocol.DeviceWeb:
		return "" // globe
	default:
		return "" // generic display
	}
}

// Device list column widths (impala-style table).
const (
	colName = 30
	colType = 14
)

// deviceHeader is the dim column-header row shown above the device list.
func deviceHeader() string {
	h := lipgloss.NewStyle().Foreground(muted)
	return "  " +
		h.Width(colName).Render("Name") +
		h.Width(colType).Render("Type") +
		h.Render("Address")
}

// deviceDelegate renders each device as one aligned table row, themed to the
// active Omarchy palette:  ▌  <icon> alias   type   ip
// icons is false on terminals without a Nerd Font (the glyphs are dropped).
type deviceDelegate struct{ icons bool }

func (deviceDelegate) Height() int                         { return 1 }
func (deviceDelegate) Spacing() int                        { return 0 }
func (deviceDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }

func (d deviceDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	it, ok := item.(peerItem)
	if !ok {
		return
	}
	dt := string(it.p.Info.DeviceType)
	if dt == "" {
		dt = "device"
	}
	name := truncate(it.p.Info.Alias, colName-2)
	if d.icons {
		name = fmt.Sprintf("%s  %s", deviceIcon(it.p.Info.DeviceType), truncate(it.p.Info.Alias, colName-4))
	}

	nameSt := lipgloss.NewStyle().Width(colName)
	typeSt := lipgloss.NewStyle().Width(colType)
	if index == m.Index() {
		fmt.Fprint(w, lipgloss.NewStyle().Foreground(accent).Render("▌ ")+
			nameSt.Foreground(accent).Bold(true).Render(name)+
			typeSt.Foreground(accent).Render(dt)+
			lipgloss.NewStyle().Foreground(accent).Render(it.p.IP))
		return
	}
	fmt.Fprint(w, "  "+
		nameSt.Foreground(text).Render(name)+
		typeSt.Foreground(dim).Render(dt)+
		lipgloss.NewStyle().Foreground(muted).Render(it.p.IP))
}
