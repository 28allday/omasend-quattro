package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// ExpandHome resolves the ~-forms a user may type or hand-edit, and leaves
// everything else untouched.
func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	cases := map[string]string{
		"~":                 home,
		"~/Omarchy-Send":    filepath.Join(home, "Omarchy-Send"),
		"~/a/b":             filepath.Join(home, "a", "b"),
		"/abs/path":         "/abs/path",
		"relative/path":     "relative/path",
		"~user/not-ours":    "~user/not-ours", // ~user expansion is not supported
		"mid/~/not-leading": "mid/~/not-leading",
		"":                  "",
	}
	for in, want := range cases {
		if got := ExpandHome(in); got != want {
			t.Errorf("ExpandHome(%q) = %q, want %q", in, got, want)
		}
	}
}

// A config file whose receiveDir was stored as "~/…" is normalised to an
// absolute path by Load — the regression that sent files into a literal "~"
// directory under the process cwd. The seed lives in the legacy omarchy-send
// dir, so this also covers first-run migration: Load reads the legacy file
// and persists the normalised config to the new omasend dir.
func TestLoadExpandsTildeReceiveDir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)

	dir := filepath.Join(cfgHome, "omarchy-send")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	seed := map[string]any{"alias": "t", "receiveDir": "~/Omarchy-Send"}
	data, _ := json.Marshal(seed)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := filepath.Join(home, "Omarchy-Send")
	if cfg.ReceiveDir != want {
		t.Fatalf("ReceiveDir = %q, want %q", cfg.ReceiveDir, want)
	}

	// And the normalised value is what got persisted — to the NEW config dir,
	// migrated off the legacy seed.
	raw, err := os.ReadFile(filepath.Join(cfgHome, "omasend", "config.json"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var onDisk map[string]any
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if onDisk["receiveDir"] != want {
		t.Fatalf("persisted receiveDir = %q, want %q", onDisk["receiveDir"], want)
	}
}
