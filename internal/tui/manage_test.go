package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"omasend/internal/config"
)

// manageModel returns a model whose receive dir is a temp dir seeded with the
// given filenames, already switched to the Manage tab.
func manageModel(t *testing.T, names ...string) (Model, string) {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Config{Alias: "omarchy", Port: 53317, ReceiveDir: dir, Protocol: "https"}
	m := New(cfg, nil)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = nm.(Model)
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("3")}) // enter Manage
	return nm.(Model), dir
}

func key(m Model, s string) Model {
	var msg tea.KeyMsg
	switch s {
	case " ":
		msg = tea.KeyMsg{Type: tea.KeySpace}
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
	nm, _ := m.Update(msg)
	return nm.(Model)
}

func TestManageListsReceivedFilesSkippingPartials(t *testing.T) {
	m, _ := manageModel(t, "photo.dng", "clip.mp4", "incoming.iso.part")
	got := len(m.fileList.Items())
	if got != 2 {
		t.Fatalf("expected 2 listed files (.part skipped), got %d", got)
	}
	out := m.View()
	for _, want := range []string{"Manage", "photo.dng", "clip.mp4"} {
		if !strings.Contains(out, want) {
			t.Errorf("manage view missing %q", want)
		}
	}
	if strings.Contains(out, ".part") {
		t.Error("in-progress .part file should not be shown")
	}
}

func TestManageDeleteSingleViaConfirm(t *testing.T) {
	m, dir := manageModel(t, "keep.txt", "drop.txt")
	// Cursor starts on the newest (drop.txt was written last). Marking it and
	// confirming should remove exactly that file.
	m = key(m, " ") // mark file under cursor
	if len(m.marked) != 1 {
		t.Fatalf("expected 1 marked, got %d", len(m.marked))
	}
	m = key(m, "d") // request delete -> confirm card
	if !m.confirmDel {
		t.Fatal("expected confirm card to be showing")
	}
	m = key(m, "y") // confirm
	if m.confirmDel {
		t.Error("confirm card should be dismissed after delete")
	}
	if len(m.fileList.Items()) != 1 {
		t.Fatalf("expected 1 file left, got %d", len(m.fileList.Items()))
	}
	// One file gone, one remains on disk.
	remaining, _ := os.ReadDir(dir)
	if len(remaining) != 1 {
		t.Fatalf("expected 1 file on disk, got %d", len(remaining))
	}
}

func TestManageDeleteAllAndCancel(t *testing.T) {
	m, dir := manageModel(t, "a.txt", "b.txt", "c.txt")
	m = key(m, "a") // mark all
	if len(m.marked) != 3 {
		t.Fatalf("expected 3 marked, got %d", len(m.marked))
	}
	m = key(m, "d")
	m = key(m, "n") // cancel
	if m.confirmDel {
		t.Error("cancel should dismiss the confirm card")
	}
	if files, _ := os.ReadDir(dir); len(files) != 3 {
		t.Fatalf("cancel must not delete anything; have %d files", len(files))
	}
	// Now actually delete all.
	m = key(m, "d")
	m = key(m, "y")
	if files, _ := os.ReadDir(dir); len(files) != 0 {
		t.Fatalf("expected all files deleted, %d remain", len(files))
	}
	if len(m.fileList.Items()) != 0 {
		t.Errorf("list should be empty after deleting all")
	}
}
