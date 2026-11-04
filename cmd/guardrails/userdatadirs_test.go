package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The binary was renamed from claude-tools to guardrails, but users' data stays where it was:
// the pii-key under <state>/claude-tools and config-gate snapshots under
// <state>/claude-tools/config-gate. Renaming either dir would orphan existing keys (every PII
// tag changes) and snapshots.
func TestUserStateDirsKeepOldName(t *testing.T) {
	for _, xdg := range []bool{false, true} {
		home := t.TempDir()
		state := filepath.Join(home, ".local", "state")
		t.Setenv("HOME", home)
		t.Setenv("XDG_STATE_HOME", "")
		if xdg {
			state = filepath.Join(t.TempDir(), "xdgstate")
			t.Setenv("XDG_STATE_HOME", state)
		}
		keyPath := filepath.Join(state, "claude-tools", "pii-key")
		if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
			t.Fatal(err)
		}
		want := "existing-user-key-0123456789abcdef"
		if err := os.WriteFile(keyPath, []byte(want), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := string(piiKey()); got != want {
			t.Errorf("xdg=%v: piiKey() = %q, want the existing key at %s", xdg, got, keyPath)
		}
		if _, err := os.Stat(filepath.Join(state, "guardrails")); err == nil {
			t.Errorf("xdg=%v: piiKey created %s/guardrails", xdg, state)
		}

		cfg := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", cfg)
		if err := os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"hooks":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		in := filepath.Join(t.TempDir(), "in.json")
		os.WriteFile(in, []byte(`{"hook_event_name":"SessionStart","source":"startup","cwd":"`+t.TempDir()+`"}`), 0o600)
		f, err := os.Open(in)
		if err != nil {
			t.Fatal(err)
		}
		old := os.Stdin
		os.Stdin = f
		rc := configGate(nil)
		os.Stdin = old
		f.Close()
		if rc != 0 {
			t.Fatalf("xdg=%v: configGate rc=%d", xdg, rc)
		}
		snaps, _ := filepath.Glob(filepath.Join(state, "claude-tools", "config-gate", "*.json"))
		if len(snaps) == 0 {
			t.Errorf("xdg=%v: no config-gate snapshot under %s/claude-tools/config-gate", xdg, state)
		}
	}
}

// The default secret list masks Bash reads of the pii-key, so its entry must name the file
// piiKey actually uses.
func TestSecretDefaultsCoverPIIKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	piiKey()
	key := filepath.Join(home, ".local", "state", "claude-tools", "pii-key")
	if _, err := os.Stat(key); err != nil {
		t.Fatalf("piiKey did not create %s: %v", key, err)
	}
	for _, p := range secretPathsDefault {
		if strings.Replace(p, "~", home, 1) == key {
			return
		}
	}
	t.Errorf("secretPathsDefault %v has no entry for %s", secretPathsDefault, key)
}
