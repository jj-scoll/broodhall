package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const baseSettings = `{
  "env": {"CLAUDE_TOOLS_PII_ALLOW": "@example.com", "OTHER": "1"},
  "permissions": {
    "allow": ["Bash(git status)", "WebSearch"],
    "deny": ["Read(~/.ssh/**)"],
    "ask": ["Bash(aws rds delete-db-instance:*)"],
    "defaultMode": "auto"
  },
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "claude-tools pre-tool-use", "onFailure": "block"}]},
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "atuin hook claude-code"}]}
    ],
    "Stop": [{"hooks": [{"type": "command", "command": "notify-send done"}]}]
  }
}`

// edit applies fn to baseSettings parsed as generic JSON and returns the result.
func edit(t *testing.T, fn func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(baseSettings), &m); err != nil {
		t.Fatal(err)
	}
	fn(m)
	b, _ := json.Marshal(m)
	return b
}

func perms(m map[string]any) map[string]any { return m["permissions"].(map[string]any) }

func TestSettingsDiffWeakened(t *testing.T) {
	cases := map[string]struct {
		fn   func(m map[string]any)
		want string
	}{
		"guard hook removed": {func(m map[string]any) {
			pre := m["hooks"].(map[string]any)["PreToolUse"].([]any)
			m["hooks"].(map[string]any)["PreToolUse"] = pre[1:]
		}, "hook removed: PreToolUse [Bash] claude-tools pre-tool-use"},
		"guard matcher narrowed": {func(m map[string]any) {
			m["hooks"].(map[string]any)["PreToolUse"].([]any)[0].(map[string]any)["matcher"] = "Nothing"
		}, "hook removed: PreToolUse [Bash] claude-tools pre-tool-use"},
		"all hooks removed": {func(m map[string]any) { delete(m, "hooks") }, "hook removed: PreToolUse [Bash] claude-tools pre-tool-use"},
		// Settings can't say whether a PreToolUse hook guards or only logs, so every one counts.
		"logging PreToolUse hook removed": {func(m map[string]any) {
			m["hooks"].(map[string]any)["PreToolUse"] = m["hooks"].(map[string]any)["PreToolUse"].([]any)[:1]
		}, "hook removed: PreToolUse [Bash] atuin hook claude-code"},
		"onFailure dropped": {func(m map[string]any) {
			h := m["hooks"].(map[string]any)["PreToolUse"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
			delete(h, "onFailure")
		}, "onFailure:block dropped: PreToolUse [Bash] claude-tools pre-tool-use"},
		"disableAllHooks":   {func(m map[string]any) { m["disableAllHooks"] = true }, "disableAllHooks set"},
		"bypassPermissions": {func(m map[string]any) { perms(m)["defaultMode"] = "bypassPermissions" }, "defaultMode set to bypassPermissions"},
		"deny removed":      {func(m map[string]any) { perms(m)["deny"] = []any{} }, "deny rule removed: Read(~/.ssh/**)"},
		"ask removed":       {func(m map[string]any) { delete(perms(m), "ask") }, "ask rule removed: Bash(aws rds delete-db-instance:*)"},
		"guard env changed": {func(m map[string]any) {
			m["env"].(map[string]any)["CLAUDE_TOOLS_PII_ALLOW"] = "@"
		}, "env CLAUDE_TOOLS_PII_ALLOW changed"},
		"guard env removed": {func(m map[string]any) { delete(m, "env") }, "env CLAUDE_TOOLS_PII_ALLOW changed"},
		"guard env added": {func(m map[string]any) {
			m["env"].(map[string]any)["CLAUDE_TOOLS_SECRET_PATHS"] = "/dev/null"
		}, "env CLAUDE_TOOLS_SECRET_PATHS changed"},
		"file deleted": {nil, "hook removed: PreToolUse [Bash] claude-tools pre-tool-use"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var next []byte
			if c.fn != nil {
				next = edit(t, c.fn)
			}
			weak, _, err := settingsDiff([]byte(baseSettings), next)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(weak, "\n"), c.want) {
				t.Errorf("weakened = %q, want it to hold %q", weak, c.want)
			}
		})
	}
}

func TestSettingsDiffNotWeakened(t *testing.T) {
	cases := map[string]func(m map[string]any){
		"unchanged":           func(m map[string]any) {},
		"non-guard hook gone": func(m map[string]any) { delete(m["hooks"].(map[string]any), "Stop") },
		"other env changed":   func(m map[string]any) { m["env"].(map[string]any)["OTHER"] = "2" },
		"deny added":          func(m map[string]any) { perms(m)["deny"] = []any{"Read(~/.ssh/**)", "Read(.env)"} },
		"mode to acceptEdits": func(m map[string]any) { perms(m)["defaultMode"] = "acceptEdits" },
		"allow rule removed":  func(m map[string]any) { perms(m)["allow"] = []any{} },
		"unknown key added":   func(m map[string]any) { m["someFutureKey"] = map[string]any{"x": 1} },
		"hook gains onFailure": func(m map[string]any) {
			m["hooks"].(map[string]any)["Stop"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["onFailure"] = "block"
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			weak, egress, err := settingsDiff([]byte(baseSettings), edit(t, fn))
			if err != nil || len(weak) != 0 || len(egress) != 0 {
				t.Errorf("weak %q egress %q err %v, want none", weak, egress, err)
			}
		})
	}
}

func TestAllowEgress(t *testing.T) {
	egress := []string{
		"Bash", "Bash(*)", "Bash(curl *)", "Bash(curl:*)", "Bash(wget -q *)", "Bash(gh api *)", "Bash(gh:*)",
		"Bash(gh pr create:*)", "Bash(gh issue comment *)", "Bash(git *)", "Bash(git push:*)", "Bash(git:*)",
		"Bash(aws s3 cp:*)", "Bash(aws *)", "Bash(aws sns publish *)", "Bash(scp *)", "Bash(rsync -av *)",
		"Bash(ssh host *)", "Bash(nc *)", "Bash(python3 *)", "Bash(node:*)", "Bash(bash -c *)", "Bash(xargs *)",
		"Bash(/usr/bin/curl *)", "WebFetch", "WebFetch(domain:example.com)",
		"mcp__claude_ai_Gmail__send_message", "mcp__claude_ai_Atlassian__addCommentToJiraIssue",
		"mcp__claude_ai_Gmail", "mcp__claude_ai_Gmail__*",
		"Bash(FOO=1 curl *)", "Bash(aws s3 sync *)", "Bash(aws s3)", "Bash(aws s3api put-object *)",
		"mcp__claude_ai_Atlassian__transitionJiraIssue",
	}
	quiet := []string{
		"Bash(git status)", "Bash(git -C /x status flakes/)", "Bash(git log:*)", "Bash(gh pr view *)", "Bash(gh run list:*)",
		"Bash(aws ec2 describe-instances:*)", "Bash(mkdir:*)", "Bash(journalctl *)", "Bash(nix build *)",
		"WebSearch", "Read(//tmp/**)", "Edit", "mcp__plugin_notes_search__search",
		"mcp__claude_ai_Atlassian__getConfluencePage",
		// From a real allow list: an env prefix, a read-verb tool that names a write, an s3 read.
		"Bash(SHELL=/bin/bash /opt/tools/bin/devbox run:*)",
		"mcp__claude_ai_Atlassian__getTransitionsForJiraIssue", "Bash(aws s3 ls *)",
	}
	for _, r := range egress {
		if !allowEgress(r) {
			t.Errorf("allowEgress(%q) = false, want true", r)
		}
	}
	for _, r := range quiet {
		if allowEgress(r) {
			t.Errorf("allowEgress(%q) = true, want false", r)
		}
	}
}

func TestSettingsDiffEgressOnlyNew(t *testing.T) {
	next := edit(t, func(m map[string]any) {
		perms(m)["allow"] = []any{"Bash(git status)", "WebSearch", "Bash(curl *)", "Bash(mkdir:*)", "Bash(curl *)"}
	})
	weak, egress, err := settingsDiff([]byte(baseSettings), next)
	if err != nil || len(weak) != 0 || strings.Join(egress, "|") != "Bash(curl *)" {
		t.Errorf("weak %q egress %q err %v, want egress [Bash(curl *)]", weak, egress, err)
	}
}

// gateInput is a ConfigChange (or SessionStart) hook payload.
func gateInput(event, source, path, cwd string) []byte {
	b, _ := json.Marshal(map[string]any{"hook_event_name": event, "session_id": "s", "cwd": cwd, "source": source, "file_path": path})
	return b
}

func TestConfigGateRun(t *testing.T) {
	newEnv := func(t *testing.T) (home, settings, state string) {
		home = t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
		settings = filepath.Join(home, ".claude", "settings.json")
		os.WriteFile(settings, []byte(baseSettings), 0o644)
		return home, settings, filepath.Join(home, "state")
	}
	run := func(t *testing.T, in []byte, state string) (int, string) {
		t.Helper()
		var stderr bytes.Buffer
		return configGateRun(in, state, &stderr), stderr.String()
	}
	weakened := edit(t, func(m map[string]any) { delete(m, "hooks") })
	curl := edit(t, func(m map[string]any) { perms(m)["allow"] = []any{"Bash(curl *)"} })

	t.Run("no snapshot yet: allow and take one", func(t *testing.T) {
		_, settings, state := newEnv(t)
		os.WriteFile(settings, weakened, 0o644)
		if code, msg := run(t, gateInput("ConfigChange", "user_settings", settings, "/"), state); code != 0 {
			t.Fatalf("exit %d %q, want 0", code, msg)
		}
		os.WriteFile(settings, []byte(baseSettings), 0o644)
		// The snapshot is now the weakened file, so restoring the hooks reads as adding hooks:
		// the gate cannot tell a restore from a hook that overrides the guards.
		if code, msg := run(t, gateInput("ConfigChange", "user_settings", settings, "/"), state); code != 2 || !strings.Contains(msg, "hook added: PreToolUse") {
			t.Errorf("restoring hooks: exit %d %q, want 2 for the added PreToolUse hook", code, msg)
		}
	})
	t.Run("weakening a snapshotted file blocks and keeps the snapshot", func(t *testing.T) {
		home, settings, state := newEnv(t)
		if code, _ := run(t, gateInput("SessionStart", "startup", "", home), state); code != 0 {
			t.Fatalf("SessionStart exit %d", code)
		}
		os.WriteFile(settings, weakened, 0o644)
		code, msg := run(t, gateInput("ConfigChange", "user_settings", settings, home), state)
		if code != 2 || !strings.Contains(msg, "hook removed: PreToolUse [Bash] claude-tools pre-tool-use") {
			t.Fatalf("exit %d %q, want 2 with the removed hook", code, msg)
		}
		if code, _ := run(t, gateInput("ConfigChange", "user_settings", settings, home), state); code != 2 {
			t.Errorf("second edit against the same snapshot: exit %d, want 2", code)
		}
	})
	t.Run("compact or clear keeps the snapshot: the process still has the old settings", func(t *testing.T) {
		home, settings, state := newEnv(t)
		run(t, gateInput("SessionStart", "startup", "", home), state)
		os.WriteFile(settings, weakened, 0o644)
		run(t, gateInput("ConfigChange", "user_settings", settings, home), state)
		for _, src := range []string{"compact", "clear", "resume"} {
			run(t, gateInput("SessionStart", src, "", home), state)
		}
		if code, _ := run(t, gateInput("ConfigChange", "user_settings", settings, home), state); code != 2 {
			t.Errorf("exit %d, want 2: a later SessionStart re-snapshotted the blocked file", code)
		}
	})
	t.Run("new egress allow reports, applies and moves the snapshot", func(t *testing.T) {
		home, settings, state := newEnv(t)
		run(t, gateInput("SessionStart", "startup", "", home), state)
		os.WriteFile(settings, curl, 0o644)
		code, msg := run(t, gateInput("ConfigChange", "user_settings", settings, home), state)
		if code != 1 || !strings.Contains(msg, "+ Bash(curl *)") {
			t.Fatalf("exit %d %q, want 1 listing Bash(curl *)", code, msg)
		}
		if code, msg := run(t, gateInput("ConfigChange", "user_settings", settings, home), state); code != 0 {
			t.Errorf("same content again: exit %d %q, want 0", code, msg)
		}
	})
	t.Run("staged proposal is diffed against its target", func(t *testing.T) {
		home, settings, state := newEnv(t)
		prop := settings + ".proposed-0a1b2c3d"
		os.WriteFile(prop, weakened, 0o644)
		code, msg := run(t, gateInput("ConfigChange", "user_settings", prop, home), state)
		if code != 2 || !strings.Contains(msg, "hook removed") {
			t.Errorf("exit %d %q, want 2", code, msg)
		}
	})
	t.Run("symlinked config dir shares the snapshot", func(t *testing.T) {
		home, settings, state := newEnv(t)
		run(t, gateInput("SessionStart", "startup", "", home), state)
		work := filepath.Join(home, ".claude-alt")
		os.MkdirAll(work, 0o755)
		os.Symlink(settings, filepath.Join(work, "settings.json"))
		os.WriteFile(settings, weakened, 0o644)
		if code, _ := run(t, gateInput("ConfigChange", "user_settings", filepath.Join(work, "settings.json"), home), state); code != 2 {
			t.Errorf("exit %d, want 2", code)
		}
	})
	t.Run("deleted settings file blocks", func(t *testing.T) {
		home, settings, state := newEnv(t)
		run(t, gateInput("SessionStart", "startup", "", home), state)
		os.Remove(settings)
		if code, _ := run(t, gateInput("ConfigChange", "user_settings", settings, home), state); code != 2 {
			t.Errorf("exit %d, want 2", code)
		}
	})
	t.Run("project settings in cwd are snapshotted at SessionStart", func(t *testing.T) {
		home, _, state := newEnv(t)
		proj := filepath.Join(home, "src", "app")
		os.MkdirAll(filepath.Join(proj, ".claude"), 0o755)
		local := filepath.Join(proj, ".claude", "settings.local.json")
		os.WriteFile(local, []byte(baseSettings), 0o644)
		run(t, gateInput("SessionStart", "startup", "", proj), state)
		os.WriteFile(local, weakened, 0o644)
		if code, _ := run(t, gateInput("ConfigChange", "local_settings", local, proj), state); code != 2 {
			t.Errorf("exit %d, want 2", code)
		}
	})
	t.Run("skills, policy and invalid JSON pass", func(t *testing.T) {
		home, settings, state := newEnv(t)
		run(t, gateInput("SessionStart", "startup", "", home), state)
		if code, _ := run(t, gateInput("ConfigChange", "skills", filepath.Join(home, ".claude", "skills", "x"), home), state); code != 0 {
			t.Errorf("skills: exit %d", code)
		}
		if code, _ := run(t, gateInput("ConfigChange", "policy_settings", "/etc/claude-code/managed-settings.json", home), state); code != 0 {
			t.Errorf("policy: exit %d", code)
		}
		os.WriteFile(settings, []byte(`{"hooks": `), 0o644)
		if code, _ := run(t, gateInput("ConfigChange", "user_settings", settings, home), state); code != 0 {
			t.Errorf("invalid JSON: exit %d, want 0 (Claude Code rejects it itself)", code)
		}
		os.WriteFile(settings, weakened, 0o644)
		if code, _ := run(t, gateInput("ConfigChange", "user_settings", settings, home), state); code != 2 {
			t.Errorf("after invalid JSON the snapshot must still be the old one: exit %d, want 2", code)
		}
	})
	t.Run("unreadable file blocks", func(t *testing.T) {
		home, _, state := newEnv(t)
		dir := filepath.Join(home, "dir-not-file")
		os.MkdirAll(dir, 0o755)
		if code, _ := run(t, gateInput("ConfigChange", "user_settings", dir, home), state); code != 2 {
			t.Errorf("exit %d, want 2 (fail closed)", code)
		}
	})
}

// pluginSettings is a settings file with the guardrails plugin enabled and its option set.
const pluginSettings = `{
  "enabledPlugins": {"guardrails@broodhall": true, "other@market": true},
  "pluginConfigs": {"guardrails@broodhall": {"options": {"disabled_guards": "pii,session-url", "allowed_email_domains": "@example.com"}}}
}`

func TestSettingsDiffPlugin(t *testing.T) {
	weakCases := map[string]struct{ next, want string }{
		"plugin disabled": {`{"enabledPlugins": {"guardrails@broodhall": false, "other@market": true}}`,
			"plugin disabled: guardrails@broodhall"},
		"plugin entry removed":   {`{"enabledPlugins": {"other@market": true}}`, "plugin disabled: guardrails@broodhall"},
		"enabledPlugins removed": {`{}`, "plugin disabled: guardrails@broodhall"},
		"family added to disabled_guards": {`{"enabledPlugins": {"guardrails@broodhall": true},
			"pluginConfigs": {"guardrails@broodhall": {"options": {"disabled_guards": "pii,session-url,egress"}}}}`,
			"guard family turned off: egress"},
		"disabled_guards whitespace and case": {`{"enabledPlugins": {"guardrails@broodhall": true},
			"pluginConfigs": {"guardrails@broodhall": {"options": {"disabled_guards": " pii , git "}}}}`,
			"guard family turned off: git"},
		"permissive PermissionRequest hook added": {`{"enabledPlugins": {"guardrails@broodhall": true},
			"hooks": {"PermissionRequest": [{"hooks": [{"type": "command", "command": "approve-all"}]}]}}`,
			"hook added: PermissionRequest [] approve-all"},
		"PreToolUse hook added": {`{"enabledPlugins": {"guardrails@broodhall": true},
			"hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "rewrite-input"}]}]}}`,
			"hook added: PreToolUse [Bash] rewrite-input"},
	}
	for name, c := range weakCases {
		t.Run("weakens/"+name, func(t *testing.T) {
			weak, _, err := settingsDiff([]byte(pluginSettings), []byte(c.next))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(weak, "\n"), c.want) {
				t.Errorf("weakened = %q, want it to hold %q", weak, c.want)
			}
		})
	}
	t.Run("weakens/option unset falls back to the default families", func(t *testing.T) {
		old := `{"enabledPlugins": {"guardrails@broodhall": true},
			"pluginConfigs": {"guardrails@broodhall": {"options": {"disabled_guards": ""}}}}`
		next := `{"enabledPlugins": {"guardrails@broodhall": true}}`
		weak, _, _ := settingsDiff([]byte(old), []byte(next))
		if !strings.Contains(strings.Join(weak, "\n"), "guard family turned off: pii") {
			t.Errorf("weakened = %q, want the default families counted as newly off", weak)
		}
	})
	okCases := map[string]string{
		"unchanged":                `{"enabledPlugins": {"guardrails@broodhall": true, "other@market": true}, "pluginConfigs": {"guardrails@broodhall": {"options": {"disabled_guards": "pii,session-url", "allowed_email_domains": "@example.com"}}}}`,
		"other plugin disabled":    `{"enabledPlugins": {"guardrails@broodhall": true, "other@market": false}, "pluginConfigs": {"guardrails@broodhall": {"options": {"disabled_guards": "pii,session-url"}}}}`,
		"family removed":           `{"enabledPlugins": {"guardrails@broodhall": true}, "pluginConfigs": {"guardrails@broodhall": {"options": {"disabled_guards": "pii"}}}}`,
		"all families turned on":   `{"enabledPlugins": {"guardrails@broodhall": true}, "pluginConfigs": {"guardrails@broodhall": {"options": {"disabled_guards": ""}}}}`,
		"email option edited":      `{"enabledPlugins": {"guardrails@broodhall": true}, "pluginConfigs": {"guardrails@broodhall": {"options": {"disabled_guards": "pii,session-url", "allowed_email_domains": "@example.org"}}}}`,
		"non-guard hook added":     `{"enabledPlugins": {"guardrails@broodhall": true}, "pluginConfigs": {"guardrails@broodhall": {"options": {"disabled_guards": "pii,session-url"}}}, "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "notify"}]}]}}`,
		"plugin enabled as a list": `{"enabledPlugins": {"guardrails@broodhall": ["1.0"]}, "pluginConfigs": {"guardrails@broodhall": {"options": {"disabled_guards": "pii,session-url"}}}}`,
	}
	for name, next := range okCases {
		t.Run("passes/"+name, func(t *testing.T) {
			weak, _, err := settingsDiff([]byte(pluginSettings), []byte(next))
			if err != nil || len(weak) != 0 {
				t.Errorf("weak %q err %v, want none", weak, err)
			}
		})
	}
}

func TestConfigGateRunPluginDisable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	settings := filepath.Join(home, ".claude", "settings.json")
	os.WriteFile(settings, []byte(pluginSettings), 0o644)
	state := filepath.Join(home, "state")
	var stderr bytes.Buffer
	configGateRun(gateInput("SessionStart", "startup", "", home), state, &stderr)
	os.WriteFile(settings, []byte(`{"enabledPlugins": {"guardrails@broodhall": false}}`), 0o644)
	if code := configGateRun(gateInput("ConfigChange", "user_settings", settings, home), state, &stderr); code != 2 || !strings.Contains(stderr.String(), "plugin disabled") {
		t.Errorf("exit %d %q, want 2 naming the disabled plugin", code, stderr.String())
	}
}

func TestConfigGateBadInputFailsClosed(t *testing.T) {
	code, _, errOut := runHook(t, configGate, "not json")
	if code != 2 || !strings.Contains(errOut, "config-gate") {
		t.Errorf("exit %d stderr %q, want 2 from config-gate", code, errOut)
	}
}
