package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"omasend/internal/config"
	"omasend/internal/discovery"
	"omasend/internal/protocol"
	"omasend/internal/transfer"
)

// fakeCtrl records Send calls for quick-send tests.
type fakeCtrl struct {
	sends     int
	sentPeer  discovery.Peer
	sentPaths []string
}

func (f *fakeCtrl) Announce() {}
func (f *fakeCtrl) Send(p discovery.Peer, paths []string, pin string) {
	f.sends++
	f.sentPeer = p
	f.sentPaths = paths
}
func (f *fakeCtrl) SendMessage(p discovery.Peer, text, pin string) {}
func (f *fakeCtrl) SetAutoAccept(bool)                             {}
func (f *fakeCtrl) SetAlias(string)                                {}
func (f *fakeCtrl) SetReceiveDir(string)                           {}
func (f *fakeCtrl) SetPIN(string)                                  {}
func (f *fakeCtrl) SetNotify(bool)                                 {}
func (f *fakeCtrl) AddKnownPeer(string)                            {}

// writeTree lays out a small fixture tree under a temp dir for walkIndex tests.
func writeTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mk := func(rel string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("a.txt")
	mk("sub/b.txt")
	mk(".hidden/secret.txt")    // dot-dir: skipped entirely
	mk("node_modules/junk.txt") // denylisted dir: skipped entirely
	return root
}

func TestWalkIndexSkipsNoiseAndDotDirs(t *testing.T) {
	root := writeTree(t)
	entries, trunc, err := walkIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if trunc {
		t.Fatal("did not expect truncation")
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.rel] = true
	}
	for _, want := range []string{"a.txt", "sub", filepath.Join("sub", "b.txt")} {
		if !got[want] {
			t.Errorf("expected %q in index, missing", want)
		}
	}
	for _, unwanted := range []string{".hidden", filepath.Join(".hidden", "secret.txt"), "node_modules", filepath.Join("node_modules", "junk.txt")} {
		if got[unwanted] {
			t.Errorf("did not expect %q in index", unwanted)
		}
	}
}

func TestWalkIndexExcludesRootItself(t *testing.T) {
	root := writeTree(t)
	entries, _, err := walkIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.path == root {
			t.Fatal("root should not be indexed as an entry")
		}
	}
}

// seedFinder builds a Model with a hand-set index for match/stage tests.
func seedFinder(paths ...string) Model {
	m := New(config.Config{}, nil)
	for _, p := range paths {
		m.fzfEntries = append(m.fzfEntries, fzfEntry{path: p, rel: p})
		m.fzfRels = append(m.fzfRels, p)
	}
	return m
}

func TestRecomputeMatchesFuzzyRanks(t *testing.T) {
	m := seedFinder("Documents/Q3-report.pdf", "Downloads/cat.jpg", "Projects/readme.md")

	(&m).recomputeMatches() // empty query → everything
	if len(m.fzfMatches) != 3 {
		t.Fatalf("empty query: want 3 matches, got %d", len(m.fzfMatches))
	}

	m.fzfQuery.SetValue("report")
	(&m).recomputeMatches()
	if len(m.fzfMatches) == 0 {
		t.Fatal("query 'report' matched nothing")
	}
	if best := m.fzfEntries[m.fzfMatches[0]].rel; best != "Documents/Q3-report.pdf" {
		t.Errorf("best match = %q, want the report", best)
	}
	if m.fzfCursor != 0 {
		t.Errorf("cursor should reset to best match, got %d", m.fzfCursor)
	}
}

func TestSendViewRenders(t *testing.T) {
	m := seedFinder("Documents/report.pdf", "Downloads/cat.jpg")
	m.width, m.height = 90, 28
	m.screen = screenPicker
	m.fzfRoot = "/home/test"
	(&m).recomputeMatches()
	m.fzfCursor = 1
	m.staged = []string{"/home/test/Downloads/cat.jpg"}

	out := m.View()
	if len(out) < 50 {
		t.Fatalf("send view rendered too little:\n%s", out)
	}
	if !strings.Contains(out, "cat.jpg") {
		t.Errorf("staged/listed file cat.jpg not shown in view:\n%s", out)
	}
}

func TestWithStagedFilesEnablesQuickSend(t *testing.T) {
	m := New(config.Config{}, nil, WithStagedFiles([]string{"/tmp/a.txt", "/tmp/b.txt"}))
	if !m.quickSend {
		t.Fatal("WithStagedFiles should enable quickSend")
	}
	if len(m.staged) != 2 {
		t.Fatalf("want 2 pre-staged paths, got %d", len(m.staged))
	}

	// Empty input must not flip into quick-send mode.
	plain := New(config.Config{}, nil, WithStagedFiles(nil))
	if plain.quickSend {
		t.Error("empty WithStagedFiles should be a no-op")
	}
}

func TestQuickSendSelectingDeviceSends(t *testing.T) {
	fc := &fakeCtrl{}
	m := New(config.Config{}, fc, WithStagedFiles([]string{"/tmp/a.txt", "/tmp/b.txt"}))

	peer := discovery.Peer{Info: protocol.DeviceInfo{Alias: "Target", Fingerprint: "fp1"}}
	m.peers["fp1"] = peer
	m.peerList.SetItems(m.peerItems())

	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := nm.(Model)

	if fc.sends != 1 {
		t.Fatalf("want exactly 1 send, got %d", fc.sends)
	}
	if len(fc.sentPaths) != 2 {
		t.Errorf("want 2 paths sent, got %d", len(fc.sentPaths))
	}
	if fc.sentPeer.Info.Alias != "Target" {
		t.Errorf("sent to %q, want Target", fc.sentPeer.Info.Alias)
	}
	if got.screen != screenTransfers {
		t.Errorf("after quick-send want screenTransfers, got %v", got.screen)
	}
	if got.quickSend {
		t.Error("quickSend should reset after sending")
	}
	if got.sendPeer == nil || got.sendPaths == nil {
		t.Error("sendPeer/sendPaths should be retained for a possible PIN retry")
	}
}

func TestQuickSendAutoQuitOnlyWhenAllDone(t *testing.T) {
	m := New(config.Config{}, nil, WithStagedFiles([]string{"/tmp/a.txt"}))
	if !m.quitAfterSend {
		t.Fatal("WithStagedFiles should arm quitAfterSend")
	}

	// Active transfer: do not schedule a quit.
	m.applyTransfer(transfer.Event{ID: "1", Dir: transfer.Outgoing, Kind: transfer.Start, Total: 10, Received: 0})
	if cmd := m.maybeScheduleQuit(); cmd != nil {
		t.Fatal("should not schedule quit while a transfer is active")
	}

	// A second file starts; first finishes — still one active, no quit.
	m.applyTransfer(transfer.Event{ID: "2", Dir: transfer.Outgoing, Kind: transfer.Start, Total: 10})
	m.applyTransfer(transfer.Event{ID: "1", Kind: transfer.FileDone})
	if m.allSendsDoneOK() {
		t.Fatal("not all sends are done yet (file 2 active)")
	}

	// Both done → schedules a quit.
	m.applyTransfer(transfer.Event{ID: "2", Kind: transfer.FileDone})
	if cmd := m.maybeScheduleQuit(); cmd == nil {
		t.Fatal("should schedule quit once all sends are done")
	}

	// An errored transfer must NOT auto-quit (leave the box open).
	em := New(config.Config{}, nil, WithStagedFiles([]string{"/tmp/x"}))
	em.applyTransfer(transfer.Event{ID: "9", Dir: transfer.Outgoing, Kind: transfer.Error})
	if em.allSendsDoneOK() {
		t.Error("errored transfer should not count as done")
	}
}

func TestDirsOnlyFiltersToFolders(t *testing.T) {
	m := New(config.Config{}, nil)
	m.fzfEntries = []fzfEntry{
		{path: "/r/Photos", rel: "Photos", dir: true},
		{path: "/r/Photos/a.jpg", rel: "Photos/a.jpg"},
		{path: "/r/notes.txt", rel: "notes.txt"},
	}
	m.fzfRels = []string{"Photos", "Photos/a.jpg", "notes.txt"}

	(&m).recomputeMatches()
	if len(m.fzfMatches) != 3 {
		t.Fatalf("default: want all 3, got %d", len(m.fzfMatches))
	}

	m.fzfDirsOnly = true
	(&m).recomputeMatches()
	if len(m.fzfMatches) != 1 {
		t.Fatalf("dirs-only: want 1 folder, got %d", len(m.fzfMatches))
	}
	if got := m.fzfEntries[m.fzfMatches[0]].rel; got != "Photos" {
		t.Errorf("dirs-only kept %q, want Photos", got)
	}
}

func TestToggleStageCursorAddsAndRemoves(t *testing.T) {
	m := seedFinder("a.txt", "b.txt")
	(&m).recomputeMatches()

	m.fzfCursor = 1
	(&m).toggleStageCursor()
	if !contains(m.staged, "b.txt") {
		t.Fatal("toggle should stage b.txt")
	}
	(&m).toggleStageCursor()
	if contains(m.staged, "b.txt") {
		t.Fatal("second toggle should unstage b.txt")
	}
}
