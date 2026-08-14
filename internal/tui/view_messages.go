package tui

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"omasend/internal/server"
)

// msgItem adapts a received message to bubbles/list.Item.
type msgItem struct{ m server.ReceivedMessage }

func (i msgItem) Title() string       { return i.m.From }
func (i msgItem) Description() string { return i.m.Text }
func (i msgItem) FilterValue() string { return i.m.From + " " + i.m.Text }

// msgItems builds list items from the model's messages (kept newest-first).
func (m Model) msgItems() []list.Item {
	items := make([]list.Item, len(m.messages))
	for i, msg := range m.messages {
		items[i] = msgItem{m: msg}
	}
	return items
}

// Message list column widths.
const (
	colFrom = 22
	colTime = 7
)

// messageHeader is the dim column-header row above the message list.
func messageHeader() string {
	h := lipgloss.NewStyle().Foreground(muted)
	return "  " +
		h.Width(colFrom).Render("From") +
		h.Width(colTime).Render("Time") +
		h.Render("Message")
}

// msgDelegate renders one message as a row:  ▌ from   15:04   preview…
type msgDelegate struct{}

func (msgDelegate) Height() int                         { return 1 }
func (msgDelegate) Spacing() int                        { return 0 }
func (msgDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }

func (msgDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	it, ok := item.(msgItem)
	if !ok {
		return
	}
	from := truncate(nonEmpty(it.m.From, "unknown"), colFrom-2)
	ts := it.m.Time.Format("15:04")
	preview := truncate(oneLine(it.m.Text), 60)

	fromSt := lipgloss.NewStyle().Width(colFrom)
	timeSt := lipgloss.NewStyle().Width(colTime)
	if index == m.Index() {
		fmt.Fprint(w, lipgloss.NewStyle().Foreground(accent).Render("▌ ")+
			fromSt.Foreground(accent).Bold(true).Render(from)+
			timeSt.Foreground(accent).Render(ts)+
			lipgloss.NewStyle().Foreground(accent).Render(preview))
		return
	}
	fmt.Fprint(w, "  "+
		fromSt.Foreground(text).Render(from)+
		timeSt.Foreground(dim).Render(ts)+
		lipgloss.NewStyle().Foreground(muted).Render(preview))
}

// composeView is the message-compose card.
func (m Model) composeView() string {
	to := ""
	if m.composeTo != nil {
		to = m.composeTo.Info.Alias
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render("Message to " + to))
	b.WriteString("\n\n")
	b.WriteString(m.composeInput.View())
	return b.String()
}

// readMessageView shows the full text of the message being read.
func (m Model) readMessageView() string {
	if m.readingMsg == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render("Message from " + nonEmpty(m.readingMsg.From, "unknown")))
	b.WriteString("  ")
	b.WriteString(headerStyle.Render(m.readingMsg.Time.Format("2006-01-02 15:04")))
	b.WriteString("\n\n")
	b.WriteString(lipgloss.NewStyle().Width(56).Foreground(text).Render(m.readingMsg.Text))
	return b.String()
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// oneLine collapses newlines so a multi-line message previews on one row.
func oneLine(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " "))
}
