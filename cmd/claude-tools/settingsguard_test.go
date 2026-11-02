package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runToolHookOut is runToolHook returning stdout, where an ask reply goes.
func runToolHookOut(t *testing.T, home, tool string, input map[string]any) (int, string) {
	t.Helper()
	in, _ := json.Marshal(map[string]any{"session_id": "s", "cwd": home, "tool_name": tool, "tool_input": input})
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	var stdout, stderr bytes.Buffer
	code := preToolUseRun(in, "CT", func([]byte) ([]byte, []byte, int) { return nil, nil, 0 }, &stdout, &stderr)
	return code, stdout.String() + stderr.String()
}

func TestSettingsGuardAsksOnWrites(t *testing.T) {
	asked := []string{
		`jq '.permissions.allow += ["Bash(curl *)"]' ~/.claude/settings.json > /tmp/s && mv /tmp/s ~/.claude/settings.json`,
		"sed -i 's/block/x/' ~/.claude/settings.json",
		"echo '{}' > $HOME/.claude/settings.local.json",
		"echo '{}' > /home/user/.claude-work/settings.json",
		"cp /tmp/loose.json ~/.claude/settings.json",
		"rm ~/.claude/settings.json",
		"echo x | tee .claude/settings.local.json",
		"cd ~/.claude && sed -i 's/a/b/' settings.json",
		`python3 -c "open('/work/.claude/settings.json','w').write('{}')"`,
		"ln -sf /tmp/x ~/.claude/settings.json",
		"rm -rf ~/.claude",
		"find ~/.claude -name settings.json -delete",
		`find ~/.claude -name 'settings*.json' -exec rm {} \;`,
		`find ~/.claude -maxdepth 1 -exec sed -i 's/a/b/' {} +`,
	}
	for _, cmd := range asked {
		t.Run(cmd, func(t *testing.T) {
			d := guardCmd(cmd)
			if !d.Ask || !strings.HasPrefix(d.Rule, "settings:") || !strings.Contains(d.Msg, "hooks and permissions") {
				t.Errorf("got %s msg %q, want ask:settings:*", d.Kind(), d.Msg)
			}
		})
	}
}

func TestSettingsGuardAllowsReads(t *testing.T) {
	allowed := []string{
		"cat ~/.claude/settings.json",
		"jq '.hooks | keys' ~/.claude/settings.json",
		"jq . ~/.claude/settings.json > /tmp/settings-copy.json",
		"cp ~/.claude/settings.json ~/.claude/settings.json.bak-x",
		"ls -la ~/.claude",
		"rg -n onFailure ~/.claude/settings.json",
		"diff ~/.claude/settings.json ~/.claude/settings.json.pre-onfailure",
		"sed -i 's/a/b/' ~/src/app/settings.json",
		"echo '{}' > config/settings.json",
		// From guard-replay: rmdir removes only empty dirs; find -exec ls only reads.
		"git worktree prune && rmdir .claude/worktrees .claude && git worktree list",
		`ls -la ~/.claude/ | head -60; echo ---; find ~/.claude -maxdepth 1 -type l -exec ls -l {} \;`,
	}
	for _, cmd := range allowed {
		t.Run(cmd, func(t *testing.T) {
			// settingsGuard alone: find ~/.claude also meets the secret-file guard (.credentials.json).
			if d := settingsGuard(&guardEnv{Cwd: "/work", Home: "/home/user"}, cmd); d.Ask || d.Exit != 0 {
				t.Errorf("got %s msg %q, want allow", d.Kind(), d.Msg)
			}
		})
	}
}

func TestSettingsGuardAsksOnEditTools(t *testing.T) {
	home := t.TempDir()
	claude := filepath.Join(home, ".claude")
	os.MkdirAll(claude, 0o755)
	file := filepath.Join(claude, "settings.json")
	os.WriteFile(file, []byte("{}"), 0o644)
	work := filepath.Join(home, ".claude-work")
	os.MkdirAll(work, 0o755)
	os.Symlink(file, filepath.Join(work, "settings.json"))
	os.Symlink(file, filepath.Join(home, "innocent.json"))

	for name, p := range map[string]string{
		"user":       file,
		"tilde":      "~/.claude/settings.json",
		"local":      filepath.Join(claude, "settings.local.json"),
		"work link":  filepath.Join(work, "settings.json"),
		"symlink":    filepath.Join(home, "innocent.json"),
		"project":    filepath.Join(home, "src", "app", ".claude", "settings.json"),
		"relative":   ".claude/settings.local.json",
		"not yet in": filepath.Join(home, "new", ".claude", "settings.local.json"),
	} {
		for _, tool := range []string{"Edit", "Write", "MultiEdit"} {
			t.Run(tool+"/"+name, func(t *testing.T) {
				code, out := runToolHookOut(t, home, tool, map[string]any{"file_path": p, "content": "{}"})
				if code != 0 || !strings.Contains(out, `"ask"`) || !strings.Contains(out, "hooks and permissions") {
					t.Errorf("exit %d out %q, want ask", code, out)
				}
			})
		}
	}
	t.Run("other files and Read pass", func(t *testing.T) {
		for tool, p := range map[string]string{
			"Write": filepath.Join(home, "src", "app", "settings.json"),
			"Edit":  filepath.Join(claude, "CLAUDE.md"),
			"Read":  file,
		} {
			if code, out := runToolHookOut(t, home, tool, map[string]any{"file_path": p}); code != 0 || strings.Contains(out, `"ask"`) {
				t.Errorf("%s %s: exit %d out %q, want allow", tool, p, code, out)
			}
		}
	})
}
