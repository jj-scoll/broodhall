package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var (
	builtOnce sync.Once
	builtBin  string
	builtErr  error
)

// builtBinary compiles guardrails once per test run and returns its path.
func builtBinary(t *testing.T) string {
	t.Helper()
	builtOnce.Do(func() {
		dir, err := os.MkdirTemp("", "guardrails-bin-")
		if err != nil {
			builtErr = err
			return
		}
		builtBin = filepath.Join(dir, "guardrails")
		if out, err := exec.Command("go", "build", "-o", builtBin, ".").CombinedOutput(); err != nil {
			builtErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if builtErr != nil {
		t.Fatal(builtErr)
	}
	return builtBin
}

// installAt copies the built binary to path, creating parent dirs.
func installAt(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(builtBinary(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

// statuslineAt runs the binary at bin the way settings.json does, with HANDOFF_STATE_DIR
// unset (set==false) or set to val, and returns its output.
func statuslineAt(t *testing.T, bin string, set bool, val string) string {
	t.Helper()
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "HANDOFF_STATE_DIR=") && !strings.HasPrefix(e, "HOME=") &&
			!strings.HasPrefix(e, "CLAUDE_CONFIG_DIR=") && !strings.HasPrefix(e, "STATUSLINE_NOW=") {
			env = append(env, e)
		}
	}
	env = append(env, "HOME=/nohome", "CLAUDE_CONFIG_DIR=", fmt.Sprintf("STATUSLINE_NOW=%d", slNow))
	if set {
		env = append(env, "HANDOFF_STATE_DIR="+val)
	}
	c := exec.Command(bin, "statusline")
	c.Env = env
	c.Stdin = strings.NewReader(slFull(""))
	out, err := c.Output()
	if err != nil {
		t.Fatalf("statusline: %v", err)
	}
	return string(out)
}

// The status line is started straight from settings.json, not through run.sh, so
// HANDOFF_STATE_DIR is unset. It must still find the state run.sh's hooks keep in
// <data>/claude-handoff, where <data>/bin/<version>/guardrails is the binary.
func TestStateDirFromExecutableLocation(t *testing.T) {
	// "/nohome"-style fake HOMEs and symlinked temp dirs (macOS /var -> /private/var) are
	// both covered: the data dir is reached through a symlink.
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(real, "plugins", "data", "guardrails-broodhall")
	installAt(t, filepath.Join(data, "bin", "0.0.0", "guardrails"))
	state := filepath.Join(data, "claude-handoff")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	slMark(t, state, "writing", fmt.Sprint(slNow-23), 23)

	other := filepath.Join(t.TempDir(), "elsewhere", "guardrails")
	installAt(t, other)

	viaLink := filepath.Join(link, "plugins", "data", "guardrails-broodhall", "bin", "0.0.0", "guardrails")
	tests := []struct {
		name string
		bin  string
		set  bool
		val  string
		want bool
	}{
		{"unset, binary in <data>/bin/<ver>", filepath.Join(data, "bin", "0.0.0", "guardrails"), false, "", true},
		{"unset, binary reached through a symlink", viaLink, false, "", true},
		{"unset, binary outside a plugin data dir", other, false, "", false},
		{"explicit empty value wins (-> /tmp)", filepath.Join(data, "bin", "0.0.0", "guardrails"), true, "", false},
		{"explicit value wins", filepath.Join(data, "bin", "0.0.0", "guardrails"), true, t.TempDir(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := statuslineAt(t, tt.bin, tt.set, tt.val)
			if got := strings.Contains(out, "handoff: writing"); got != tt.want {
				t.Errorf("handoff segment shown = %v, want %v\n%s", got, tt.want, out)
			}
		})
	}
}

func TestPluginDataBase(t *testing.T) {
	tests := []struct{ exe, want string }{
		{"/h/.claude/plugins/data/guardrails-broodhall/bin/1.6.0/guardrails", "/h/.claude/plugins/data/guardrails-broodhall"},
		{"/h/.claude/plugins/data/guardrails/bin/1.6.0/guardrails", "/h/.claude/plugins/data/guardrails"},
		{"/h/.claude/plugins/data/other/bin/1.6.0/guardrails", ""},
		// The binary's old name: an older plugin version's binary keeps its state dir.
		{"/h/.claude/plugins/data/guardrails-broodhall/bin/0.3.2/claude-tools", "/h/.claude/plugins/data/guardrails-broodhall"},
		{"/h/.claude/plugins/data/guardrails/bin/1.0.0/claude-tools", "/h/.claude/plugins/data/guardrails"},
		{"/h/.claude/plugins/data/other/bin/1.0.0/claude-tools", ""},
		{"/h/.claude/plugins/data/guardrails-x/bin/claude-tools", ""},
		{"/usr/local/bin/claude-tools", ""},
		{"/h/.claude/plugins/data/guardrails-x/bin/1.0.0/other", ""},
	}
	for _, tt := range tests {
		if got := pluginDataBase(tt.exe); got != tt.want {
			t.Errorf("pluginDataBase(%q) = %q, want %q", tt.exe, got, tt.want)
		}
	}
}
