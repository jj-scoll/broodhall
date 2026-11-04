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
		"echo '{}' > /nohome/user/.claude-work/settings.json",
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

// A Claude config dir is the session's CLAUDE_CONFIG_DIR, whatever its name, or a dir right in
// home named .claude*: the same rule as the secret-file guard's credentials.
func TestSettingsGuardFollowsConfigDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "claude"))
	for name, p := range map[string]string{
		"CLAUDE_CONFIG_DIR":  filepath.Join(home, "claude", "settings.json"),
		"tilde config dir":   "~/claude/settings.local.json",
		"~/.claude-personal": filepath.Join(home, ".claude-personal", "settings.json"),
		"~/.claude-work":     filepath.Join(home, ".claude-work", "settings.json"),
	} {
		t.Run("Edit/"+name, func(t *testing.T) {
			code, out := runToolHookOut(t, home, "Edit", map[string]any{"file_path": p})
			if code != 0 || !strings.Contains(out, `"ask"`) {
				t.Errorf("exit %d out %q, want ask", code, out)
			}
		})
	}
	for name, p := range map[string]string{
		"other dir in home":      filepath.Join(home, "claude-notes", "settings.json"),
		".claude-* below home":   filepath.Join(home, "src", "app", ".claude-plugin", "settings.json"),
		"subdir of a config dir": filepath.Join(home, "claude", "x", "settings.json"),
	} {
		t.Run("Edit passes/"+name, func(t *testing.T) {
			if code, out := runToolHookOut(t, home, "Edit", map[string]any{"file_path": p}); code != 0 || strings.Contains(out, `"ask"`) {
				t.Errorf("exit %d out %q, want allow", code, out)
			}
		})
	}
	env := &guardEnv{Cwd: "/work", Home: home}
	for _, cmd := range []string{
		"echo '{}' > ~/claude/settings.json",
		"sed -i 's/a/b/' ~/.claude-personal/settings.local.json",
		"cd ~/claude && sed -i 's/a/b/' settings.json",
		"rm -rf ~/claude",
		// cd and pushd move where a relative path points (main asked for the first by name).
		"cd ~ && echo '{}' > .claude-work/settings.json",
		"cd ~ && sed -i s/a/b/ .claude-personal/settings.json",
		"pushd ~ && cd .claude-personal && sed -i s/a/b/ settings.json",
		"(cd ~ && rm -rf .claude-work)",
		// The target is the first word that is not an option or a redirect; bare cd is ~.
		"pushd ~ >/dev/null && echo '{}' > .claude-work/settings.json",
		"pushd ~ > /dev/null; echo '{}' > .claude-work/settings.json",
		"cd ~ 2>/dev/null && echo '{}' > .claude-work/settings.json",
		"cd -P ~ &>/dev/null && sed -i s/a/b/ .claude-personal/settings.json",
		"cd && echo '{}' > .claude-work/settings.json",
		// Where cd went is unknown: a relative .claude* path is also read as ~/.claude*.
		"cd - && echo '{}' > .claude-work/settings.json",
		`cd "$PROJ" && echo '{}' > .claude-personal/settings.json`,
		"cd $(git rev-parse --show-toplevel) && echo '{}' > .claude-work/settings.json",
		"popd && echo '{}' > .claude-work/settings.json",
		// The variable as written: the hook has the same CLAUDE_CONFIG_DIR as the shell.
		"echo '{}' > $CLAUDE_CONFIG_DIR/settings.json",
		"sed -i s/a/b/ ${CLAUDE_CONFIG_DIR}/settings.local.json",
	} {
		t.Run("Bash/"+cmd, func(t *testing.T) {
			if d := settingsGuard(env, cmd); !d.Ask || !strings.HasPrefix(d.Rule, "settings:") {
				t.Errorf("got %s, want ask:settings:*", d.Kind())
			}
		})
	}
	for _, cmd := range []string{
		"echo '{}' > ~/claude-notes/settings.json",
		"cd ~/src && echo '{}' > .claude-work/settings.json",
		"cd ~/src 2>/dev/null && echo '{}' > .claude-work/settings.json",
		"pushd ~/src >/dev/null && echo '{}' > .claude-work/settings.json",
		"cd - && echo '{}' > notes/settings.json",
		"echo '{}' > $CLAUDE_CONFIG_DIR_OLD/settings.json",
	} {
		if d := settingsGuard(env, cmd); d.Ask {
			t.Errorf("%q: got %s, want allow", cmd, d.Kind())
		}
	}
}

// The same rule when home or CLAUDE_CONFIG_DIR is a symlink: the real path counts, and a
// command naming the real config dir is not skipped.
func TestSettingsGuardSymlinkedDirs(t *testing.T) {
	real := t.TempDir()
	home := filepath.Join(t.TempDir(), "homelink")
	os.Symlink(real, home)
	os.MkdirAll(filepath.Join(real, ".claude-work"), 0o755)
	os.MkdirAll(filepath.Join(real, "claude2"), 0o755)
	os.Symlink(filepath.Join(real, "claude2"), filepath.Join(real, "cfglink"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "cfglink"))
	t.Run("Edit real path under a symlinked home", func(t *testing.T) {
		code, out := runToolHookOut(t, home, "Edit", map[string]any{"file_path": filepath.Join(real, ".claude-work", "settings.json")})
		if code != 0 || !strings.Contains(out, `"ask"`) {
			t.Errorf("exit %d out %q, want ask", code, out)
		}
	})
	t.Run("rm the real dir of a symlinked CLAUDE_CONFIG_DIR", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(real, "cfglink"))
		if d := settingsGuard(&guardEnv{Cwd: "/work", Home: real}, "rm -rf ~/claude2"); !d.Ask {
			t.Errorf("got %s, want ask", d.Kind())
		}
	})
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
			if d := settingsGuard(&guardEnv{Cwd: "/work", Home: "/nohome/user"}, cmd); d.Ask || d.Exit != 0 {
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
