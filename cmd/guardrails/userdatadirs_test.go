package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The binary was renamed from claude-tools to guardrails. New data goes under
// <base>/guardrails, but a user's existing data stays where it was: a pii-key under
// <state>/claude-tools (a new key would change every PII tag), config-gate snapshots, the
// rules file and secret-paths under <config>/claude-tools.
func TestUserPathPrefersNewName(t *testing.T) {
	for _, tt := range []struct {
		name     string
		existing []string // dirs under base that hold "f"
		want     string
	}{
		{"fresh install", nil, "guardrails"},
		{"only the old name", []string{"claude-tools"}, "claude-tools"},
		{"only the new name", []string{"guardrails"}, "guardrails"},
		{"both: new wins", []string{"claude-tools", "guardrails"}, "guardrails"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			for _, d := range tt.existing {
				os.MkdirAll(filepath.Join(base, d), 0o700)
				os.WriteFile(filepath.Join(base, d, "f"), nil, 0o600)
			}
			if got, want := userPath(base, "f"), filepath.Join(base, tt.want, "f"); got != want {
				t.Errorf("userPath = %q, want %q", got, want)
			}
		})
	}
	t.Run("candidates list both, new first", func(t *testing.T) {
		got := userPaths("/b", "f")
		if len(got) != 2 || got[0] != "/b/guardrails/f" || got[1] != "/b/claude-tools/f" {
			t.Errorf("userPaths = %v", got)
		}
	})
}

// GUARDRAILS_<NAME> wins whenever it is set, even empty (the launcher always exports it);
// CLAUDE_TOOLS_<NAME> is read only when the new name is unset.
func TestSettingEnv(t *testing.T) {
	t.Setenv("CLAUDE_TOOLS_X_TEST", "old")
	os.Unsetenv("GUARDRAILS_X_TEST")
	if got := settingEnv("X_TEST"); got != "old" {
		t.Errorf("new unset: got %q, want the old name's value", got)
	}
	t.Setenv("GUARDRAILS_X_TEST", "new")
	if got := settingEnv("X_TEST"); got != "new" {
		t.Errorf("both set: got %q, want new", got)
	}
	t.Setenv("GUARDRAILS_X_TEST", "")
	if got := settingEnv("X_TEST"); got != "" {
		t.Errorf("new set empty: got %q, want empty", got)
	}
}

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
		if err := os.MkdirAll(filepath.Join(state, "claude-tools", "config-gate"), 0o700); err != nil {
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
		if n := runConfigGateStartup(t); n == 0 {
			t.Fatalf("xdg=%v: configGate failed", xdg)
		}
		snaps, _ := filepath.Glob(filepath.Join(state, "claude-tools", "config-gate", "*.json"))
		if len(snaps) == 0 {
			t.Errorf("xdg=%v: no config-gate snapshot under %s/claude-tools/config-gate", xdg, state)
		}
	}
}

func TestUserStateDirsFreshInstallUseNewName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	state := filepath.Join(home, ".local", "state")
	k1 := piiKey()
	if _, err := os.Stat(filepath.Join(state, "guardrails", "pii-key")); err != nil {
		t.Fatalf("piiKey did not create the key under guardrails: %v", err)
	}
	if string(piiKey()) != string(k1) {
		t.Errorf("second piiKey call made a different key")
	}
	if _, err := os.Stat(filepath.Join(state, "claude-tools")); err == nil {
		t.Errorf("fresh install created %s/claude-tools", state)
	}
	runConfigGateStartup(t)
	if snaps, _ := filepath.Glob(filepath.Join(state, "guardrails", "config-gate", "*.json")); len(snaps) == 0 {
		t.Errorf("no config-gate snapshot under %s/guardrails/config-gate", state)
	}
}

// runConfigGateStartup runs the SessionStart config gate on a minimal settings dir and
// returns 1 on success.
func runConfigGateStartup(t *testing.T) int {
	t.Helper()
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
		t.Fatalf("configGate rc=%d", rc)
	}
	return 1
}

// The default secret list masks Bash reads of the pii-key, so it must name the file piiKey
// uses, under either name.
func TestSecretDefaultsCoverPIIKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	piiKey()
	for _, dir := range []string{"guardrails", "claude-tools"} {
		key := filepath.Join(home, ".local", "state", dir, "pii-key")
		if dir == "guardrails" {
			if _, err := os.Stat(key); err != nil {
				t.Fatalf("piiKey did not create %s: %v", key, err)
			}
		}
		found := false
		for _, p := range secretPathsDefault {
			found = found || strings.Replace(p, "~", home, 1) == key
		}
		if !found {
			t.Errorf("secretPathsDefault %v has no entry for %s", secretPathsDefault, key)
		}
	}
}
