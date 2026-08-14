// Package tui implements the Bubble Tea front end. It routes between Devices,
// Transfers, Manage (received-file housekeeping), Messages and Settings screens,
// offers a recursive fuzzy file finder for sending, and raises modal prompts for
// incoming files and for confirming deletions.
package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"omasend/internal/app"
	"omasend/internal/clipboard"
	"omasend/internal/config"
	"omasend/internal/discovery"
	"omasend/internal/server"
	"omasend/internal/theme"
	"omasend/internal/transfer"
)

// Controller lets the TUI drive the network layer.
type Controller interface {
	Announce()
	Send(peer discovery.Peer, paths []string, pin string)
	SendMessage(peer discovery.Peer, text, pin string)
	SetAutoAccept(bool)
	SetAlias(string)
	SetReceiveDir(string)
	SetPIN(string)
	SetNotify(bool)
	AddKnownPeer(host string) // probe a remote host directly (off-LAN / Tailscale)
}

type screen int

const (
	screenPeers screen = iota
	screenTransfers
	screenManage
	screenMessages
	screenSettings
	screenPicker // entered from Peers; not part of the tab rotation

	tabCount = 5 // Devices · Transfers · Manage · Messages · Settings
)

// xfer is one row in the transfers view.
type xfer struct {
	key      string
	name     string
	dir      transfer.Direction
	received int64
	total    int64
	state    string // active | done | error | cancelled
	started  time.Time
}

// finished reports whether the transfer is in a terminal state.
func (x *xfer) finished() bool {
	return x.state == "done" || x.state == "error" || x.state == "cancelled"
}

// Model is the root router model.
type Model struct {
	cfg    config.Config
	ctrl   Controller
	ips    []string
	screen screen

	peerList list.Model
	peers    map[string]discovery.Peer

	// Send screen: a recursive fuzzy file finder ("fzf") rooted at fzfRoot.
	fzfQuery    textinput.Model // the fuzzy query input
	fzfRoot     string          // directory currently indexed
	fzfEntries  []fzfEntry      // files+dirs found under fzfRoot
	fzfRels     []string        // entry paths relative to fzfRoot (match corpus)
	fzfMatches  []int           // indexes into fzfEntries, best-ranked first
	fzfCursor   int             // selected row within fzfMatches
	fzfIndexing bool            // a walk is in flight
	fzfTrunc    bool            // the walk hit the entry cap
	fzfDirsOnly bool            // show only folders (pick a whole dir to send)
	fzfErr      string          // last walk error

	staged []string        // file/dir paths queued to send
	target *discovery.Peer // peer we're sending to

	bar       progress.Model
	transfers []*xfer
	xferIndex map[string]*xfer

	pending *server.AcceptRequest // non-nil while an accept prompt is showing

	// Manage (received files) tab.
	fileList   list.Model
	marked     map[string]bool // paths queued for deletion, shared with fileDelegate
	manageErr  string          // last load/delete error, shown beneath the list
	confirmDel bool            // a delete-confirmation card is showing
	delTargets []string        // paths the pending confirmation will delete

	autoAccept bool

	// Settings edit form.
	editing    bool
	editFocus  int
	editInputs []textinput.Model // 0=alias, 1=receive dir, 2=pin

	// PIN prompt state for sending to a PIN-protected peer. pendingMsg is set
	// (and sendPaths nil) when the pending send is a message rather than files,
	// so the PIN retry resends the right thing.
	pinInput   textinput.Model
	showPin    bool
	sendPeer   *discovery.Peer
	sendPaths  []string
	pendingMsg string

	// Add-remote-peer modal (Devices tab): host/IP/Tailscale-name entry.
	addingPeer bool
	peerInput  textinput.Model

	// Messages tab + compose modal.
	msgList      list.Model
	messages     []server.ReceivedMessage
	composing    bool
	composeInput textinput.Model
	composeTo    *discovery.Peer
	readingMsg   *server.ReceivedMessage // non-nil while reading a message full-screen
	notice       string                  // transient footer flash (e.g. "message sent")

	// quickSend is set by WithStagedFiles: the TUI opens with files already
	// staged and selecting a device on the Devices screen sends them straight
	// away (used by the Nautilus right-click integration).
	quickSend bool
	// quitAfterSend (also set by WithStagedFiles) auto-closes the window once
	// the outgoing transfer completes, so the right-click box doesn't linger.
	// quitPending guards against scheduling more than one quit tick at a time.
	quitAfterSend bool
	quitPending   bool

	width, height int
	quitting      bool
}

// Option customizes a Model at construction.
type Option func(*Model)

// WithStagedFiles opens the TUI in quick-send mode: paths are pre-staged and
// picking a device on the Devices screen sends them immediately, skipping the
// file finder. Empty input is a no-op (normal startup).
func WithStagedFiles(paths []string) Option {
	return func(m *Model) {
		if len(paths) == 0 {
			return
		}
		m.staged = append([]string(nil), paths...)
		m.quickSend = true
		m.quitAfterSend = true
	}
}

// New returns the root model. ctrl may be nil (e.g. in tests).
func New(cfg config.Config, ctrl Controller, opts ...Option) Model {
	applyTheme(theme.Load()) // match the active Omarchy theme

	l := list.New(nil, deviceDelegate{icons: !cfg.NoIcons}, 0, 0)
	l.SetShowTitle(false)
	l.SetShowHelp(false)
	l.SetShowStatusBar(false)

	marked := make(map[string]bool)
	fl := list.New(nil, fileDelegate{marked: marked}, 0, 0)
	fl.SetShowTitle(false)
	fl.SetShowHelp(false)
	fl.SetShowStatusBar(false)

	ml := list.New(nil, msgDelegate{}, 0, 0)
	ml.SetShowTitle(false)
	ml.SetShowHelp(false)
	ml.SetShowStatusBar(false)

	fzfQuery := textinput.New()
	fzfQuery.Prompt = "› "
	fzfQuery.Placeholder = "type to fuzzy-find files…"
	fzfQuery.CharLimit = 128
	fzfQuery.Width = 48
	fzfQuery.PromptStyle = lipgloss.NewStyle().Foreground(accent)

	pin := textinput.New()
	pin.Placeholder = "PIN"
	pin.CharLimit = 16

	compose := textinput.New()
	compose.Placeholder = "type a message"
	compose.CharLimit = 2000
	compose.Width = 48

	peerInput := textinput.New()
	peerInput.Placeholder = "host, IP, or Tailscale name"
	peerInput.CharLimit = 256
	peerInput.Width = 48

	mkInput := func(placeholder string, limit int) textinput.Model {
		ti := textinput.New()
		ti.Placeholder = placeholder
		ti.CharLimit = limit
		ti.Width = 48
		return ti
	}
	editInputs := []textinput.Model{
		mkInput("alias", 63),
		mkInput("receive directory", 256),
		mkInput("PIN (blank = disabled)", 16),
	}

	m := Model{
		cfg:          cfg,
		ctrl:         ctrl,
		ips:          server.LocalIPs(),
		screen:       screenPeers,
		peerList:     l,
		peers:        make(map[string]discovery.Peer),
		fileList:     fl,
		marked:       marked,
		fzfQuery:     fzfQuery,
		bar:          progress.New(progress.WithDefaultGradient(), progress.WithWidth(22)),
		xferIndex:    make(map[string]*xfer),
		autoAccept:   cfg.AutoAccept,
		pinInput:     pin,
		editInputs:   editInputs,
		msgList:      ml,
		composeInput: compose,
		peerInput:    peerInput,
	}
	for _, o := range opts {
		o(&m)
	}
	return m
}

func (m Model) Init() tea.Cmd { return nil }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		iw, ih := innerDims(msg.Width, msg.Height)
		lh := ih - 2 // leave room for the column-header row + blank
		if lh < 1 {
			lh = 1
		}
		m.peerList.SetSize(iw, lh)
		m.fileList.SetSize(iw, lh)
		m.msgList.SetSize(iw, lh)
		if qw := iw - 4; qw > 8 {
			m.fzfQuery.Width = qw
		}
		return m, nil

	case app.PeerFoundMsg:
		m.peers[msg.Peer.Info.Fingerprint] = msg.Peer
		m.peerList.SetItems(m.peerItems())
		return m, nil

	case app.PeerLostMsg:
		delete(m.peers, msg.Fingerprint)
		m.peerList.SetItems(m.peerItems())
		return m, nil

	case app.IncomingMsg:
		req := msg.Req
		m.pending = &req
		return m, nil

	case app.TransferMsg:
		// A PIN-required signal opens the PIN prompt instead of a transfer row.
		if msg.Ev.Kind == transfer.Error && errors.Is(msg.Ev.Err, transfer.ErrPinRequired) {
			m.showPin = true
			m.pinInput.SetValue("")
			m.pinInput.Focus()
			return m, textinput.Blink
		}
		m.applyTransfer(msg.Ev)
		return m, m.maybeScheduleQuit()

	case autoQuitMsg:
		// The debounce elapsed — quit only if the send is still complete (a new
		// file may have started since, e.g. mid-folder).
		m.quitPending = false
		if m.allSendsDoneOK() {
			m.quitting = true
			return m, tea.Quit
		}
		return m, nil

	case app.MessageMsg:
		m.messages = append([]server.ReceivedMessage{msg.Msg}, m.messages...)
		m.msgList.SetItems(m.msgItems())
		m.notice = "✉ message from " + nonEmpty(msg.Msg.From, "a device")
		return m, nil

	case fzfIndexedMsg:
		if msg.root != m.fzfRoot {
			return m, nil // a stale walk for a root we've since left
		}
		m.fzfIndexing = false
		m.fzfEntries = msg.entries
		m.fzfTrunc = msg.trunc
		if msg.err != nil {
			m.fzfErr = msg.err.Error()
		} else {
			m.fzfErr = ""
		}
		m.fzfRels = make([]string, len(msg.entries))
		for i, e := range msg.entries {
			m.fzfRels[i] = e.rel
		}
		m.recomputeMatches()
		return m, nil

	case tea.KeyMsg:
		m.notice = "" // any key clears a transient footer notice
		if m.composing {
			return m.updateCompose(msg)
		}
		if m.addingPeer {
			return m.updateAddPeer(msg)
		}
		if m.readingMsg != nil {
			switch msg.String() {
			case "esc", "q", "enter":
				m.readingMsg = nil
			case "y":
				if err := clipboard.Write(m.readingMsg.Text); err != nil {
					m.notice = "clipboard unavailable"
				} else {
					m.notice = "copied to clipboard"
				}
			}
			return m, nil
		}
		if m.showPin {
			return m.updatePin(msg)
		}
		if m.editing {
			return m.updateSettingsEdit(msg)
		}
		if m.confirmDel {
			return m.updateConfirmDelete(msg)
		}
		if m.pending != nil {
			return m.updateAccept(msg)
		}
		if m.screen == screenPicker {
			return m.updateFzf(msg)
		}
		if m.screen == screenPeers && m.peerList.FilterState() == list.Filtering {
			break
		}
		if m.screen == screenManage && m.fileList.FilterState() == list.Filtering {
			break
		}
		switch msg.String() {
		case "q", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case "1":
			m.screen = screenPeers
			return m, nil
		case "2":
			m.screen = screenTransfers
			return m, nil
		case "3":
			m.screen = screenManage
			m.refreshManage()
			return m, nil
		case "4":
			m.screen = screenMessages
			return m, nil
		case "5":
			m.screen = screenSettings
			return m, nil
		case "tab":
			m.screen = (m.screen + 1) % tabCount
			if m.screen == screenManage {
				m.refreshManage()
			}
			return m, nil
		case "shift+tab":
			m.screen = (m.screen + tabCount - 1) % tabCount
			if m.screen == screenManage {
				m.refreshManage()
			}
			return m, nil
		case "+":
			// Add a remote device by host/IP/Tailscale-name (off-LAN peer).
			if m.screen == screenPeers {
				m.addingPeer = true
				m.peerInput.SetValue("")
				m.peerInput.Focus()
				return m, textinput.Blink
			}
			return m, nil
		case "r":
			if m.screen == screenPeers && m.ctrl != nil {
				m.ctrl.Announce()
			}
			if m.screen == screenManage {
				m.refreshManage()
			}
			return m, nil
		case "c":
			if m.screen == screenTransfers {
				m.clearFinished()
			}
			return m, nil
		case " ":
			if m.screen == screenManage {
				m.toggleMark()
			}
			return m, nil
		case "d", "x":
			if m.screen == screenManage {
				return m.requestDelete()
			}
			if m.screen == screenMessages {
				m.deleteSelectedMessage()
			}
			return m, nil
		case "m":
			// Compose a message to the highlighted device.
			if m.screen == screenPeers {
				if it, ok := m.peerList.SelectedItem().(peerItem); ok {
					peer := it.p
					m.composeTo = &peer
					m.composing = true
					m.composeInput.SetValue("")
					m.composeInput.Focus()
					return m, textinput.Blink
				}
			}
			return m, nil
		case "v":
			// Send the clipboard: open compose to the selected device,
			// pre-filled with the clipboard text, so it can be reviewed first.
			if m.screen == screenPeers {
				if it, ok := m.peerList.SelectedItem().(peerItem); ok {
					text, err := clipboard.Read()
					if err != nil {
						m.notice = "clipboard unavailable"
						return m, nil
					}
					if strings.TrimSpace(text) == "" {
						m.notice = "clipboard is empty"
						return m, nil
					}
					peer := it.p
					m.composeTo = &peer
					m.composing = true
					m.composeInput.SetValue(text)
					m.composeInput.CursorEnd()
					m.composeInput.Focus()
					return m, textinput.Blink
				}
			}
			return m, nil
		case "y":
			// Copy the selected/open message to the clipboard.
			if m.screen == screenMessages {
				var text string
				if m.readingMsg != nil {
					text = m.readingMsg.Text
				} else if it, ok := m.msgList.SelectedItem().(msgItem); ok {
					text = it.m.Text
				}
				if text == "" {
					return m, nil
				}
				if err := clipboard.Write(text); err != nil {
					m.notice = "clipboard unavailable"
				} else {
					m.notice = "copied to clipboard"
				}
			}
			return m, nil
		case "a":
			if m.screen == screenManage {
				m.toggleMarkAll()
				return m, nil
			}
			if m.screen == screenSettings {
				m.autoAccept = !m.autoAccept
				if m.ctrl != nil {
					m.ctrl.SetAutoAccept(m.autoAccept)
				}
			}
			return m, nil
		case "i":
			if m.screen == screenSettings {
				m.cfg.NoIcons = !m.cfg.NoIcons
				m.peerList.SetDelegate(deviceDelegate{icons: !m.cfg.NoIcons})
				_ = m.cfg.Save()
			}
			return m, nil
		case "n":
			if m.screen == screenSettings {
				m.cfg.NoNotify = !m.cfg.NoNotify
				if m.ctrl != nil {
					m.ctrl.SetNotify(!m.cfg.NoNotify)
				}
				_ = m.cfg.Save()
			}
			return m, nil
		case "e":
			if m.screen == screenSettings {
				return m.beginEdit()
			}
			return m, nil
		case "enter":
			if m.screen == screenPeers {
				if it, ok := m.peerList.SelectedItem().(peerItem); ok {
					peer := it.p
					// Quick-send (Nautilus right-click): files are already
					// staged — selecting a device sends them straight away. A
					// PIN-required peer still routes through the PIN prompt via
					// sendPeer/sendPaths.
					if m.quickSend && len(m.staged) > 0 && m.ctrl != nil {
						m.sendPeer = &peer
						m.sendPaths = m.staged
						m.pendingMsg = ""
						m.ctrl.Send(peer, m.staged, "")
						m.staged = nil
						m.quickSend = false
						m.screen = screenTransfers
						return m, nil
					}
					m.target = &peer
					m.staged = nil
					home, err := os.UserHomeDir()
					if err != nil || home == "" {
						home = "."
					}
					m.fzfRoot = home
					m.fzfQuery.SetValue("")
					m.fzfQuery.Focus()
					m.fzfEntries, m.fzfRels, m.fzfMatches, m.fzfCursor = nil, nil, nil, 0
					m.fzfErr = ""
					m.fzfDirsOnly = false
					m.fzfIndexing = true
					m.screen = screenPicker
					return m, tea.Batch(indexFiles(home), textinput.Blink)
				}
			}
			if m.screen == screenMessages {
				if it, ok := m.msgList.SelectedItem().(msgItem); ok {
					msg := it.m
					m.readingMsg = &msg
				}
				return m, nil
			}
		}
	}

	if m.addingPeer {
		var cmd tea.Cmd
		m.peerInput, cmd = m.peerInput.Update(msg)
		return m, cmd
	}
	if m.pending == nil && m.screen == screenPicker {
		var cmd tea.Cmd
		m.fzfQuery, cmd = m.fzfQuery.Update(msg)
		return m, cmd
	}
	if m.pending == nil && m.screen == screenPeers {
		var cmd tea.Cmd
		m.peerList, cmd = m.peerList.Update(msg)
		return m, cmd
	}
	if m.pending == nil && !m.confirmDel && m.screen == screenManage {
		var cmd tea.Cmd
		m.fileList, cmd = m.fileList.Update(msg)
		return m, cmd
	}
	if m.pending == nil && m.screen == screenMessages {
		var cmd tea.Cmd
		m.msgList, cmd = m.msgList.Update(msg)
		return m, cmd
	}
	return m, nil
}

// updateCompose handles the message-compose modal.
func (m Model) updateCompose(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.composing = false
		m.composeTo = nil
		return m, nil
	case "enter":
		text := strings.TrimSpace(m.composeInput.Value())
		if text != "" && m.composeTo != nil && m.ctrl != nil {
			peer := *m.composeTo
			// Remember the pending message so a PIN prompt can resend it.
			m.sendPeer = &peer
			m.sendPaths = nil
			m.pendingMsg = text
			m.ctrl.SendMessage(peer, text, "")
			m.notice = "✉ message sent to " + peer.Info.Alias
		}
		m.composing = false
		m.composeTo = nil
		return m, nil
	}
	var cmd tea.Cmd
	m.composeInput, cmd = m.composeInput.Update(msg)
	return m, cmd
}

// updateAddPeer handles the add-remote-device modal. On enter it persists the
// host to config's known-peers and asks the controller to probe it now.
func (m Model) updateAddPeer(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.addingPeer = false
		return m, nil
	case "enter":
		host := strings.TrimSpace(m.peerInput.Value())
		if host != "" {
			if !contains(m.cfg.KnownPeers, host) {
				m.cfg.KnownPeers = append(m.cfg.KnownPeers, host)
				_ = m.cfg.Save()
			}
			if m.ctrl != nil {
				m.ctrl.AddKnownPeer(host)
			}
			m.notice = "added remote " + host + " — probing…"
		}
		m.addingPeer = false
		return m, nil
	}
	var cmd tea.Cmd
	m.peerInput, cmd = m.peerInput.Update(msg)
	return m, cmd
}

// deleteSelectedMessage drops the highlighted message from the list.
func (m *Model) deleteSelectedMessage() {
	it, ok := m.msgList.SelectedItem().(msgItem)
	if !ok {
		return
	}
	for i := range m.messages {
		if m.messages[i] == it.m {
			m.messages = append(m.messages[:i], m.messages[i+1:]...)
			break
		}
	}
	m.msgList.SetItems(m.msgItems())
}

// beginEdit enters the settings edit form, prefilling current values.
func (m Model) beginEdit() (tea.Model, tea.Cmd) {
	m.editing = true
	m.editFocus = 0
	m.editInputs[0].SetValue(m.cfg.Alias)
	m.editInputs[1].SetValue(m.cfg.ReceiveDir)
	m.editInputs[2].SetValue(m.cfg.PIN)
	for i := range m.editInputs {
		m.editInputs[i].Blur()
	}
	m.editInputs[0].Focus()
	return m, textinput.Blink
}

// updateSettingsEdit drives the settings edit form.
func (m Model) updateSettingsEdit(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.editing = false
		return m, nil
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "ctrl+s":
		return m.saveEdit()
	case "tab", "down":
		m.editFocus = (m.editFocus + 1) % len(m.editInputs)
		m.focusEdit()
		return m, textinput.Blink
	case "shift+tab", "up":
		m.editFocus = (m.editFocus - 1 + len(m.editInputs)) % len(m.editInputs)
		m.focusEdit()
		return m, textinput.Blink
	case "enter":
		// Enter on the last field saves; otherwise advances.
		if m.editFocus == len(m.editInputs)-1 {
			return m.saveEdit()
		}
		m.editFocus++
		m.focusEdit()
		return m, textinput.Blink
	}
	var cmd tea.Cmd
	m.editInputs[m.editFocus], cmd = m.editInputs[m.editFocus].Update(msg)
	return m, cmd
}

func (m *Model) focusEdit() {
	for i := range m.editInputs {
		if i == m.editFocus {
			m.editInputs[i].Focus()
		} else {
			m.editInputs[i].Blur()
		}
	}
}

// saveEdit persists the form to config and applies it live.
func (m Model) saveEdit() (tea.Model, tea.Cmd) {
	alias := strings.TrimSpace(m.editInputs[0].Value())
	dir := strings.TrimSpace(m.editInputs[1].Value())
	pin := strings.TrimSpace(m.editInputs[2].Value())
	if alias != "" {
		m.cfg.Alias = alias
		m.cfg.DeviceModel = alias
	}
	if dir != "" {
		// Expand a typed ~-form immediately so the live server and the saved
		// config both carry the absolute path.
		m.cfg.ReceiveDir = config.ExpandHome(dir)
	}
	m.cfg.PIN = pin
	_ = m.cfg.Save()
	if m.ctrl != nil {
		m.ctrl.SetAlias(m.cfg.Alias)
		m.ctrl.SetReceiveDir(m.cfg.ReceiveDir)
		m.ctrl.SetPIN(m.cfg.PIN)
	}
	m.editing = false
	return m, nil
}

// updatePin handles the PIN entry prompt shown when a peer needs a PIN.
func (m Model) updatePin(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		pin := strings.TrimSpace(m.pinInput.Value())
		m.showPin = false
		m.pinInput.Blur()
		if pin != "" && m.sendPeer != nil && m.ctrl != nil {
			if m.pendingMsg != "" {
				m.ctrl.SendMessage(*m.sendPeer, m.pendingMsg, pin)
				m.notice = "✉ message sent to " + m.sendPeer.Info.Alias
				m.pendingMsg = ""
			} else {
				m.ctrl.Send(*m.sendPeer, m.sendPaths, pin)
				m.screen = screenTransfers
			}
		}
		return m, nil
	case "esc", "ctrl+c":
		m.showPin = false
		m.pinInput.Blur()
		return m, nil
	}
	var cmd tea.Cmd
	m.pinInput, cmd = m.pinInput.Update(msg)
	return m, cmd
}

func (m Model) updateAccept(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y", "enter":
		m.pending.Reply <- server.AcceptDecision{Accept: true}
		m.pending = nil
		m.screen = screenTransfers
	case "n", "N", "esc":
		m.pending.Reply <- server.AcceptDecision{Accept: false}
		m.pending = nil
	}
	return m, nil
}

// autoQuitMsg fires after the post-send debounce to close a quick-send window.
type autoQuitMsg struct{}

// allSendsDoneOK reports whether every transfer has completed successfully —
// the trigger for auto-closing a quick-send window. A still-active, errored, or
// cancelled transfer (or none at all) returns false, so the window stays open
// for the user to see what happened.
func (m *Model) allSendsDoneOK() bool {
	if len(m.transfers) == 0 {
		return false
	}
	for _, x := range m.transfers {
		if x.state != "done" {
			return false
		}
	}
	return true
}

// maybeScheduleQuit arms a debounced auto-quit when a quick-send has fully
// completed. The delay lets the user glimpse the ✓ and lets the next file in a
// multi-file/folder send start before we re-check at fire time.
func (m *Model) maybeScheduleQuit() tea.Cmd {
	if !m.quitAfterSend || m.quitPending || !m.allSendsDoneOK() {
		return nil
	}
	m.quitPending = true
	return tea.Tick(1200*time.Millisecond, func(time.Time) tea.Msg { return autoQuitMsg{} })
}

func (m *Model) applyTransfer(ev transfer.Event) {
	x, ok := m.xferIndex[ev.ID]
	if !ok {
		x = &xfer{key: ev.ID, name: ev.FileName, dir: ev.Dir, total: ev.Total, state: "active", started: time.Now()}
		m.xferIndex[ev.ID] = x
		m.transfers = append(m.transfers, x)
	}
	if ev.FileName != "" {
		x.name = ev.FileName
	}
	if ev.Total > 0 {
		x.total = ev.Total
	}
	switch ev.Kind {
	case transfer.Start, transfer.Progress:
		x.received = ev.Received
	case transfer.FileDone:
		x.received = x.total
		x.state = "done"
	case transfer.Error:
		x.state = "error"
	case transfer.Cancel:
		x.state = "cancelled"
	}
}

func (m Model) peerItems() []list.Item {
	ps := make([]discovery.Peer, 0, len(m.peers))
	for _, p := range m.peers {
		ps = append(ps, p)
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Info.Alias < ps[j].Info.Alias })
	items := make([]list.Item, len(ps))
	for i, p := range ps {
		items[i] = peerItem{p: p}
	}
	return items
}

// innerDims returns the content width/height inside the bordered frame for a
// given window size: 1 title row + 1 tab row + 1 footer row, plus a 1-cell
// border and 1-cell horizontal padding on each side.
func innerDims(w, h int) (iw, ih int) {
	if w <= 0 {
		w = 90
	}
	if h <= 0 {
		h = 28
	}
	iw = w - 4
	if iw < 20 {
		iw = 20
	}
	ih = h - 5
	if ih < 3 {
		ih = 3
	}
	return iw, ih
}

func (m Model) View() string {
	if m.quitting {
		return ""
	}
	w, h := m.width, m.height
	if w <= 0 {
		w = 90
	}
	if h <= 0 {
		h = 28
	}

	// Focused prompts take over the window as a centered card.
	if m.showPin {
		return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, cardStyle.Render(m.pinView()))
	}
	if m.pending != nil {
		return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, cardStyle.Render(m.acceptView()))
	}
	if m.confirmDel {
		return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, cardStyle.Render(m.confirmDeleteView()))
	}
	if m.composing {
		return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, cardStyle.Render(m.composeView()))
	}
	if m.addingPeer {
		return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, cardStyle.Render(m.addPeerView()))
	}
	if m.readingMsg != nil {
		return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, cardStyle.Render(m.readMessageView()))
	}

	cw, ih := innerDims(w, h)
	var body string
	center := false // center short content; let populated lists fill top-down
	switch m.screen {
	case screenPeers:
		if len(m.peers) == 0 {
			msg := "Searching for devices on the network…"
			if m.quickSend {
				msg += fmt.Sprintf("\n\n%d item(s) ready — pick a device to send.", len(m.staged))
			}
			body, center = headerStyle.Render(msg), true
		} else {
			head := deviceHeader()
			if m.quickSend {
				head = titleStyle.Render(fmt.Sprintf("Send %d item(s) — select a device:", len(m.staged))) + "\n" + head
			}
			body = head + "\n" + m.peerList.View()
		}
	case screenTransfers:
		if len(m.transfers) == 0 {
			body, center = headerStyle.Render("No transfers yet.\nIncoming and outgoing files appear here."), true
		} else {
			body = transferHeader() + "\n\n" + m.transfersView()
		}
	case screenManage:
		switch {
		case m.manageErr != "" && len(m.fileList.Items()) == 0:
			body, center = lipgloss.NewStyle().Foreground(bad).Render(m.manageErr), true
		case len(m.fileList.Items()) == 0:
			body, center = headerStyle.Render("No received files.\nFiles sent to this device appear here."), true
		default:
			body = m.manageView()
		}
	case screenMessages:
		if len(m.messages) == 0 {
			body, center = headerStyle.Render("No messages yet.\nSelect a device and press m to send one."), true
		} else {
			body = messageHeader() + "\n" + m.msgList.View()
		}
	case screenSettings:
		if m.editing {
			body, center = m.settingsEditView(), true
		} else {
			body, center = m.settingsView(), true
		}
	case screenPicker:
		body = m.sendView()
	}
	if center {
		body = centerIn(cw, ih, body)
	}
	// lipgloss Width includes padding but not the border, so the frame width is
	// w-2 (border) while the content area inside the padding is w-4.
	panel := frameStyle.Width(w - 2).Height(ih).Render(body)
	return lipgloss.JoinVertical(lipgloss.Left,
		m.titleBar(w),
		m.tabBar(),
		panel,
		footerStyle.Render(m.footerText()),
	)
}

// centerIn centers a multi-line block within w×h, keeping the block's lines
// left-aligned relative to each other (it pads every line to the block's width
// first, so columns stay aligned rather than each line centering on its own).
func centerIn(w, h int, s string) string {
	block := lipgloss.NewStyle().Width(lipgloss.Width(s)).Render(s)
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, block)
}

// titleBar is the full-width accent bar: app name on the left, alias·IP on the right.
func (m Model) titleBar(w int) string {
	left := " Omarchy-Send"
	right := m.cfg.Alias
	if len(m.ips) > 0 {
		right += " · " + m.ips[0]
	}
	right += " "
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return titleBarStyle.Width(w).Render(left + strings.Repeat(" ", gap) + right)
}

func (m Model) tabBar() string {
	tab := func(label string, s screen) string {
		if m.screen == s {
			return tabActiveStyle.Render(label)
		}
		return tabInactiveStyle.Render(label)
	}
	return " " + tab("Devices", screenPeers) + tab("Transfers", screenTransfers) + tab("Manage", screenManage) + tab("Messages", screenMessages) + tab("Settings", screenSettings)
}

// stagedPanel renders the queued files as a bordered box (or a hint when empty).
func (m Model) stagedPanel() string {
	border := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(muted).
		Padding(0, 1)
	if len(m.staged) == 0 {
		return border.Render(headerStyle.Render("Nothing staged — enter stages the highlighted file or folder."))
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render(fmt.Sprintf("Staged · %d", len(m.staged))))
	for _, p := range m.staged {
		label := collapseHome(p)
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			label += "/  (folder)"
		}
		b.WriteString("\n" + valueStyle.Render("• "+label))
	}
	return border.Render(b.String())
}

func (m Model) pinView() string {
	target := ""
	if m.sendPeer != nil {
		target = m.sendPeer.Info.Alias
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render("PIN required"))
	b.WriteString("\n\n")
	b.WriteString(headerStyle.Render(target + " requires a PIN."))
	b.WriteString("\n\n")
	b.WriteString(m.pinInput.View())
	b.WriteString("\n\n")
	b.WriteString(footerStyle.Render("enter send · esc cancel"))
	return b.String()
}

func (m Model) addPeerView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Add remote device"))
	b.WriteString("\n\n")
	b.WriteString(headerStyle.Render("A device off your LAN — e.g. a Tailscale name or IP.\nIt's probed directly (no multicast) and saved."))
	b.WriteString("\n\n")
	b.WriteString(m.peerInput.View())
	b.WriteString("\n\n")
	b.WriteString(footerStyle.Render("enter add · esc cancel"))
	return b.String()
}

func (m Model) acceptView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Incoming files"))
	b.WriteString("\n\n")
	b.WriteString(valueStyle.Render(m.pending.From.Alias))
	b.WriteString(headerStyle.Render(fmt.Sprintf("  ·  %d file(s)  ·  %s", len(m.pending.Files), humanBytes(m.pending.TotalSize))))
	b.WriteString("\n\n")
	names := make([]string, 0, len(m.pending.Files))
	for _, f := range m.pending.Files {
		names = append(names, f.FileName)
	}
	sort.Strings(names)
	for _, n := range names {
		b.WriteString("  • " + n + "\n")
	}
	b.WriteString("\n")
	b.WriteString(footerStyle.Render("y/enter accept   ·   n/esc reject"))
	return b.String()
}

// transferHeader is the dim column-header row shown above the transfers list.
func transferHeader() string {
	h := lipgloss.NewStyle().Foreground(muted)
	return "  " + h.Width(24).Render("Name") + h.Width(25).Render("Progress") + h.Render("Size")
}

func (m Model) transfersView() string {
	var b strings.Builder
	for _, x := range m.transfers {
		ratio := 0.0
		if x.total > 0 {
			ratio = float64(x.received) / float64(x.total)
		}
		if ratio > 1 {
			ratio = 1
		}
		arrow := "↓"
		if x.dir == transfer.Outgoing {
			arrow = "↑"
		}
		var status string
		switch x.state {
		case "active":
			status = fmt.Sprintf("%s/%s", humanBytes(x.received), humanBytes(x.total))
		case "done":
			status = lipgloss.NewStyle().Foreground(good).Render("✓ done")
		case "error":
			status = lipgloss.NewStyle().Foreground(bad).Render("✗ error")
		case "cancelled":
			status = lipgloss.NewStyle().Foreground(muted).Render("cancelled")
		}
		name := lipgloss.NewStyle().Foreground(text).Width(22).Render(truncate(x.name, 22))
		b.WriteString(fmt.Sprintf("%s %s %s  %s\n", arrow, name, m.bar.ViewAs(ratio), status))
	}
	return b.String()
}

// rateETA returns a "1.2 MiB/s · 3s left" string for an active transfer.
func (m Model) rateETA(x *xfer) string {
	elapsed := time.Since(x.started).Seconds()
	if elapsed < 0.3 || x.received == 0 {
		return ""
	}
	rate := float64(x.received) / elapsed // bytes/sec
	out := fmt.Sprintf("%s/s", humanBytes(int64(rate)))
	if x.total > x.received && rate > 0 {
		eta := time.Duration(float64(x.total-x.received)/rate) * time.Second
		out += fmt.Sprintf(" · %s left", eta.Round(time.Second))
	}
	return out
}

// refreshManage reloads the received-files list and prunes marks for files that
// no longer exist on disk.
func (m *Model) refreshManage() {
	items, err := receivedFiles(m.cfg.ReceiveDir)
	if err != nil {
		m.manageErr = err.Error()
	} else {
		m.manageErr = ""
	}
	m.fileList.SetItems(items)
	live := make(map[string]bool, len(items))
	for _, raw := range items {
		live[raw.(fileItem).path] = true
	}
	for p := range m.marked {
		if !live[p] {
			delete(m.marked, p)
		}
	}
}

// toggleMark flips the deletion mark on the file under the cursor.
func (m *Model) toggleMark() {
	it, ok := m.fileList.SelectedItem().(fileItem)
	if !ok {
		return
	}
	if m.marked[it.path] {
		delete(m.marked, it.path)
	} else {
		m.marked[it.path] = true
	}
}

// toggleMarkAll marks every listed file, or clears all marks if they are
// already fully marked.
func (m *Model) toggleMarkAll() {
	items := m.fileList.Items()
	allMarked := len(items) > 0
	for _, raw := range items {
		if !m.marked[raw.(fileItem).path] {
			allMarked = false
			break
		}
	}
	for _, raw := range items {
		p := raw.(fileItem).path
		if allMarked {
			delete(m.marked, p)
		} else {
			m.marked[p] = true
		}
	}
}

// requestDelete gathers the deletion targets — the marked files, or the file
// under the cursor when nothing is marked — and raises the confirm card.
func (m Model) requestDelete() (tea.Model, tea.Cmd) {
	var targets []string
	for _, raw := range m.fileList.Items() {
		it := raw.(fileItem)
		if m.marked[it.path] {
			targets = append(targets, it.path)
		}
	}
	if len(targets) == 0 {
		if it, ok := m.fileList.SelectedItem().(fileItem); ok {
			targets = append(targets, it.path)
		}
	}
	if len(targets) == 0 {
		return m, nil
	}
	m.delTargets = targets
	m.confirmDel = true
	return m, nil
}

// updateConfirmDelete handles the delete-confirmation card.
func (m Model) updateConfirmDelete(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y", "enter":
		m.doDelete()
		m.confirmDel = false
		m.delTargets = nil
	case "n", "N", "esc", "ctrl+c":
		m.confirmDel = false
		m.delTargets = nil
	}
	return m, nil
}

// doDelete removes the queued targets from disk, then reloads the list. As a
// guard against deleting anything outside the receive folder, only direct
// children of it are removed.
func (m *Model) doDelete() {
	recv := filepath.Clean(expandHome(m.cfg.ReceiveDir))
	var failed []string
	for _, p := range m.delTargets {
		if filepath.Dir(p) != recv {
			failed = append(failed, filepath.Base(p))
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			failed = append(failed, filepath.Base(p))
			continue
		}
		delete(m.marked, p)
	}
	m.refreshManage()
	if len(failed) > 0 {
		m.manageErr = "failed to delete: " + strings.Join(failed, ", ")
	}
}

// clearFinished drops terminal transfers from the list.
func (m *Model) clearFinished() {
	kept := m.transfers[:0]
	for _, x := range m.transfers {
		if x.finished() {
			delete(m.xferIndex, x.key)
		} else {
			kept = append(kept, x)
		}
	}
	m.transfers = kept
}

func (m Model) settingsView() string {
	if m.editing {
		return m.settingsEditView()
	}
	var b strings.Builder
	rows := [][2]string{
		{"Alias", m.cfg.Alias},
		{"Device type", m.cfg.DeviceType},
		{"Protocol", m.cfg.Protocol},
		{"Fingerprint", m.cfg.Fingerprint},
		{"Port", fmt.Sprintf("%d", m.cfg.Port)},
		{"Receive dir", m.cfg.ReceiveDir},
		{"Auto-accept", boolStr(m.autoAccept)},
		{"PIN", boolStr(m.cfg.PIN != "")},
		{"Icons", boolStr(!m.cfg.NoIcons)},
		{"Notifications", boolStr(!m.cfg.NoNotify)},
		{"Local IPs", strings.Join(m.ips, ", ")},
	}
	for _, r := range rows {
		b.WriteString(labelStyle.Render(r[0]))
		b.WriteString(valueStyle.Render(r[1]))
		b.WriteByte('\n')
	}
	return b.String()
}

// settingsEditView renders the editable settings form.
func (m Model) settingsEditView() string {
	labels := []string{"Alias", "Receive dir", "PIN"}
	var b strings.Builder
	b.WriteString(titleStyle.Render("Edit settings"))
	b.WriteString("\n\n")
	for i, ti := range m.editInputs {
		marker := "  "
		if i == m.editFocus {
			marker = "> "
		}
		b.WriteString(marker + labelStyle.Render(labels[i]) + ti.View() + "\n")
	}
	return b.String()
}

// footerText is the contextual help line shown at the bottom of the window.
func (m Model) footerText() string {
	switch {
	case m.notice != "":
		return m.notice
	case m.composing:
		return "enter send · esc cancel"
	case m.addingPeer:
		return "enter add remote · esc cancel"
	case m.readingMsg != nil:
		return "y copy · esc/enter close"
	case m.confirmDel:
		return "y/enter delete · n/esc cancel"
	case m.editing:
		return "tab/↑↓ move · enter next · ctrl+s save · esc cancel"
	case m.screen == screenPicker:
		return "type filter · ↑↓ move · enter stage · ctrl+d folders-only · ctrl+s send · ctrl+u up-dir · esc back"
	case m.screen == screenPeers && m.quickSend:
		return fmt.Sprintf("enter send %d item(s) to selected device · r refresh · q cancel", len(m.staged))
	case m.screen == screenPeers:
		return "enter send-to · m message · v clipboard · + add remote · r refresh · / filter · q quit"
	case m.screen == screenTransfers:
		return "c clear finished · 1-5 switch · q quit"
	case m.screen == screenManage:
		return "space mark · a all · d delete · r refresh · / filter · 1-5 switch · q quit"
	case m.screen == screenMessages:
		return "enter read · y copy · d delete · 1-5 switch · q quit"
	case m.screen == screenSettings:
		return "e edit · a auto-accept · i icons · n notify · 1-5 switch · q quit"
	}
	return "q quit"
}

func boolStr(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// collapseHome shortens an absolute path under $HOME to a ~-prefixed form.
func collapseHome(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(p, home) {
		return "~" + p[len(home):]
	}
	return p
}

// expandHome resolves a leading ~ (or ~/) to the user's home directory. It is
// the inverse of collapseHome; the canonical implementation lives in config so
// every consumer of ReceiveDir expands the same way.
func expandHome(p string) string {
	return config.ExpandHome(p)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
