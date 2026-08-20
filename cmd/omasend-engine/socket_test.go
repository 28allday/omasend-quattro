package main

import (
	"net"
	"os"
	"path/filepath"

	"testing"
)

// The control socket must never land in a shared directory. The old code
// fell back to os.TempDir() when XDG_RUNTIME_DIR was unset — a predictable
// path another local user could pre-create and squat.
func TestSocketPathNeverInSharedTmp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", "")

	p, err := socketPath()
	if err != nil {
		t.Fatalf("socketPath: %v", err)
	}
	// The exact-path assertion is the shared-tmp guard: the old code
	// returned os.TempDir()/omasend-<uid>.sock here, ignoring HOME.
	want := filepath.Join(home, ".local", "state", "omasend", "omasend.sock")
	if p != want {
		t.Fatalf("socket path = %q, want %q", p, want)
	}
	fi, err := os.Lstat(filepath.Dir(p))
	if err != nil {
		t.Fatalf("socket dir: %v", err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir mode = %o, want 700", fi.Mode().Perm())
	}
}

func TestSocketPathPrefersRuntimeDir(t *testing.T) {
	rt := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", rt)
	p, err := socketPath()
	if err != nil {
		t.Fatalf("socketPath: %v", err)
	}
	if want := filepath.Join(rt, "omasend.sock"); p != want {
		t.Fatalf("socket path = %q, want %q", p, want)
	}
}

// A pre-existing fallback dir that is too open gets tightened, and a
// symlink where the dir should be is refused outright rather than followed.
func TestSocketPathHardensFallbackDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", "")

	dir := filepath.Join(home, ".local", "state", "omasend")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := socketPath(); err != nil {
		t.Fatalf("socketPath: %v", err)
	}
	fi, _ := os.Lstat(dir)
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %o after socketPath, want 700", fi.Mode().Perm())
	}

	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := socketPath(); err == nil {
		t.Fatal("socketPath accepted a symlinked socket dir")
	}
}

func TestPrepareSocketRemovesStaleKeepsLive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "omasend.sock")

	// Nothing there: fine.
	if err := prepareSocket(path); err != nil {
		t.Fatalf("empty: %v", err)
	}

	// Live engine: refused.
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareSocket(path); err == nil {
		t.Fatal("prepareSocket ignored a live listener")
	}

	// Stale file left by a crash: removed, and the path binds again.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("expected a stale socket file to remain: %v", err)
	}
	if err := prepareSocket(path); err != nil {
		t.Fatalf("stale: %v", err)
	}
	ln2, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("rebind after stale cleanup: %v", err)
	}
	ln2.Close()
}

func TestPrepareSocketRefusesNonSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "omasend.sock")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareSocket(path); err == nil {
		t.Fatal("prepareSocket removed a regular file")
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatal("the regular file was deleted")
	}
}

func TestSameUserAcceptsOwnConnection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "omasend.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	done := make(chan bool, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- false
			return
		}
		defer conn.Close()
		done <- sameUser(conn)
	}()

	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !<-done {
		t.Fatal("sameUser rejected a same-uid connection")
	}
}
