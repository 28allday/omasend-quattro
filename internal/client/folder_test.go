package client

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"omasend/internal/discovery"
	"omasend/internal/protocol"
	"omasend/internal/server"
	"omasend/internal/transfer"
)

// expand must walk a directory and advertise each file with a name relative to
// the selected folder's parent, so the folder itself is part of the path.
func TestExpandDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Trip")
	mustWrite(t, filepath.Join(dir, "cover.jpg"), "a")
	mustWrite(t, filepath.Join(dir, "day1", "img.jpg"), "bb")
	mustWrite(t, filepath.Join(dir, "day1", "notes.txt"), "ccc")

	s := New(protocol.DeviceInfo{})
	items := s.expand([]string{dir})

	got := make([]string, len(items))
	for i, it := range items {
		got[i] = it.name
	}
	sort.Strings(got)
	want := []string{"Trip/cover.jpg", "Trip/day1/img.jpg", "Trip/day1/notes.txt"}
	if len(got) != len(want) {
		t.Fatalf("expand returned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expand[%d] = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

// A regular file path passes through with just its base name.
func TestExpandFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "single.bin")
	mustWrite(t, p, "x")
	items := New(protocol.DeviceInfo{}).expand([]string{p})
	if len(items) != 1 || items[0].name != "single.bin" {
		t.Fatalf("expand file = %+v", items)
	}
}

// End-to-end: sending a directory recreates its structure on the receiver.
func TestSendDirectoryPreservesStructure(t *testing.T) {
	recvDir := t.TempDir()
	recvInfo := protocol.DeviceInfo{
		Alias: "recv", Version: protocol.ProtocolVersion, Port: 53998, Protocol: "http",
	}
	srv := server.New(server.Options{Info: recvInfo, ReceiveDir: recvDir, AutoAccept: true})
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

	srcRoot := t.TempDir()
	dir := filepath.Join(srcRoot, "Trip")
	files := map[string]string{
		"cover.jpg":        "cover-bytes",
		"day1/img.jpg":     "day1-image",
		"day2/clip.mov":    "day2-clip",
		"day2/sub/note.md": "nested-note",
	}
	for rel, body := range files {
		mustWrite(t, filepath.Join(dir, filepath.FromSlash(rel)), body)
	}

	sender := New(protocol.DeviceInfo{Alias: "sender", Fingerprint: "snd1", Version: "2.1", Protocol: "http"})
	peer := discovery.Peer{Info: recvInfo, IP: "127.0.0.1"}
	sender.Send(peer, []string{dir}, "")

	done := 0
	deadline := time.After(5 * time.Second)
	for done < len(files) {
		select {
		case ev := <-sender.Events():
			if ev.Kind == transfer.Error {
				t.Fatalf("send error: %v", ev.Err)
			}
			if ev.Kind == transfer.FileDone {
				done++
			}
		case <-deadline:
			t.Fatalf("timed out after %d/%d files", done, len(files))
		}
	}

	for rel, body := range files {
		got, err := os.ReadFile(filepath.Join(recvDir, "Trip", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("missing received %q: %v", rel, err)
		}
		if string(got) != body {
			t.Fatalf("%q content = %q, want %q", rel, got, body)
		}
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
