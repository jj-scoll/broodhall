package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// ecsEnv points every state location at temp dirs and returns the default export file.
// GUARDRAILS_ECS_EXPORT is left unset: callers turn the export on.
func ecsEnv(t *testing.T) (exportFile string) {
	t.Helper()
	t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	for _, k := range []string{"GUARDRAILS_ECS_EXPORT", "CLAUDE_TOOLS_ECS_EXPORT", "GUARDRAILS_ECS_EXPORT_PATH", "CLAUDE_TOOLS_ECS_EXPORT_PATH"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	return filepath.Join(xdg, "guardrails", "events.ecs.jsonl")
}

func ecsLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("export file: %v", err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %q is not one JSON object: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// The export is opt-in: with the option unset, or set to anything but on, nothing is written.
func TestECSExportOffByDefault(t *testing.T) {
	for _, v := range []string{"unset", "", "false", "0", "no"} {
		t.Run(v, func(t *testing.T) {
			file := ecsEnv(t)
			if v != "unset" {
				t.Setenv("GUARDRAILS_ECS_EXPORT", v)
			}
			logGuardEvent("s1", "block", "download-exec")
			if _, err := os.Lstat(file); !os.IsNotExist(err) {
				t.Errorf("export off must write nothing, stat = %v", err)
			}
			if _, err := os.Lstat(filepath.Dir(file)); !os.IsNotExist(err) {
				t.Errorf("export off must not create the state dir, stat = %v", err)
			}
		})
	}
}

// When on, each event is one ECS object of exactly this shape; outcome lines are skipped.
func TestECSExportWritesExactShape(t *testing.T) {
	file := ecsEnv(t)
	t.Setenv("GUARDRAILS_ECS_EXPORT", "true")
	old := stampVersion
	stampVersion = "v9.8.7"
	defer func() { stampVersion = old }()

	before := time.Now().UTC().Truncate(time.Second)
	logGuardEvent("sess-1", "block", "download-exec")
	logGuardEvent("sess-1", "ran", "download-exec")
	logGuardEvent("sess-1", "not-run", "git-stash")
	logGuardEvent("sess-1", "would-ask", "git-stash")
	after := time.Now().UTC()

	st, err := os.Stat(file)
	if err != nil {
		t.Fatalf("export file: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("export file mode = %v, want 0600", st.Mode().Perm())
	}
	lines := ecsLines(t, file)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2 (ran/not-run are skipped): %v", len(lines), lines)
	}
	host, _ := os.Hostname()
	u, _ := user.Current()
	for i, want := range []struct{ action, rule string }{{"block", "download-exec"}, {"would-ask", "git-stash"}} {
		got := lines[i]
		ts, _ := got["@timestamp"].(string)
		at, err := time.Parse(time.RFC3339, ts)
		if err != nil || !strings.HasSuffix(ts, "Z") || at.Before(before) || at.After(after) {
			t.Errorf("@timestamp = %q (%v), want UTC RFC 3339 between %v and %v", ts, err, before, after)
		}
		exp := map[string]any{
			"@timestamp": ts,
			"event": map[string]any{
				"kind":     "event",
				"category": []any{"intrusion_detection"},
				"provider": "guardrails",
				"dataset":  "guardrails.decisions",
				"action":   want.action,
			},
			"rule":     map[string]any{"name": want.rule, "ruleset": "guardrails"},
			"observer": map[string]any{"product": "guardrails", "vendor": "broodhall", "version": "9.8.7"},
			"labels":   map[string]any{"session_id": "sess-1"},
		}
		if host != "" {
			exp["host"] = map[string]any{"name": host}
		}
		if u != nil && u.Username != "" {
			exp["user"] = map[string]any{"name": u.Username}
		}
		if !reflect.DeepEqual(got, exp) {
			g, _ := json.Marshal(got)
			e, _ := json.Marshal(exp)
			t.Errorf("line %d:\n got %s\nwant %s", i, g, e)
		}
	}
	raw, _ := os.ReadFile(file)
	if !bytes.HasPrefix(raw, []byte(`{"@timestamp":`)) {
		t.Errorf("each line starts with @timestamp, got %.40q", raw)
	}
}

// A rule string carries the command after its first space at some call sites; only the rule id
// may reach the export, and a rule id with JSON in it stays a string value.
func TestECSExportNeverLeaksPayload(t *testing.T) {
	ecsEnv(t)
	t.Setenv("GUARDRAILS_ECS_EXPORT", "1")
	out := filepath.Join(t.TempDir(), "sub", "ecs.jsonl")
	t.Setenv("GUARDRAILS_ECS_EXPORT_PATH", out)

	logGuardEvent("s", "block", "dangerous:rm -rf /nohome/user AKIAEXAMPLESECRET")
	logGuardEvent("s", "ask", `x","labels":{"injected":"1"},"y":"`)

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("custom path: %v", err)
	}
	for _, leak := range []string{"nohome", "AKIAEXAMPLESECRET", "-rf"} {
		if bytes.Contains(raw, []byte(leak)) {
			t.Errorf("export leaks %q: %s", leak, raw)
		}
	}
	lines := ecsLines(t, out)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	if r := lines[0]["rule"].(map[string]any)["name"]; r != "dangerous:rm" {
		t.Errorf("rule.name = %v, want the rule cut at its first space", r)
	}
	if r := lines[1]["rule"].(map[string]any)["name"]; r != `x","labels":{"injected":"1"},"y":"` {
		t.Errorf("rule.name = %v, want the quoted rule id kept as one string", r)
	}
	if l := lines[1]["labels"]; !reflect.DeepEqual(l, map[string]any{"session_id": "s"}) {
		t.Errorf("labels = %v: a rule id must not add fields", l)
	}
	if _, ok := lines[1]["y"]; ok {
		t.Error("a rule id must not add top-level fields")
	}
}

// A path is "~/..." or absolute. A relative path would land in the session's cwd, so it writes nothing.
func TestECSExportPathForms(t *testing.T) {
	ecsEnv(t)
	t.Setenv("GUARDRAILS_ECS_EXPORT", "yes")
	home := os.Getenv("HOME")
	t.Setenv("GUARDRAILS_ECS_EXPORT_PATH", "~/logs/g.jsonl")
	logGuardEvent("s", "nudge", "read-dedup")
	if lines := ecsLines(t, filepath.Join(home, "logs", "g.jsonl")); len(lines) != 1 {
		t.Errorf("~ path: %d lines", len(lines))
	}

	cwd := t.TempDir()
	t.Chdir(cwd)
	t.Setenv("GUARDRAILS_ECS_EXPORT_PATH", "rel.jsonl")
	logGuardEvent("s", "nudge", "read-dedup")
	if m, _ := filepath.Glob(filepath.Join(cwd, "*")); len(m) != 0 {
		t.Errorf("relative path must write nothing, cwd holds %v", m)
	}
}

// An export that cannot be written changes nothing the hook prints or returns, and the ledger
// line is still written.
func TestECSExportUnwritableKeepsHookOutput(t *testing.T) {
	run := func(t *testing.T, cmd string) (int, string, string) {
		in, _ := json.Marshal(map[string]any{"session_id": "s", "cwd": t.TempDir(), "tool_name": "Bash",
			"tool_input": map[string]any{"command": cmd}})
		var stdout, stderr bytes.Buffer
		code := preToolUseRun(in, "CT", func([]byte) ([]byte, []byte, int) { return nil, nil, 0 }, &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}
	roDir := filepath.Join(t.TempDir(), "ro")
	os.Mkdir(roDir, 0o500)
	for _, cmd := range []string{"git push --force origin main", "git stash", "cat ~/.aws/credentials"} {
		ecsEnv(t)
		wantCode, wantOut, wantErr := run(t, cmd)
		for _, path := range []string{"/dev/null/ecs.jsonl", filepath.Join(roDir, "ecs.jsonl"), roDir} {
			ecsEnv(t)
			t.Setenv("GUARDRAILS_ECS_EXPORT", "true")
			t.Setenv("GUARDRAILS_ECS_EXPORT_PATH", path)
			code, out, errOut := run(t, cmd)
			if code != wantCode || out != wantOut || errOut != wantErr {
				t.Errorf("%q with export to %s: got (%d, %q, %q), want (%d, %q, %q)", cmd, path, code, out, errOut, wantCode, wantOut, wantErr)
			}
			if evs, _ := newGuardEvents("s"); len(evs) == 0 {
				t.Errorf("%q with export to %s: ledger line missing", cmd, path)
			}
		}
	}
}

// The launcher passes the two export options to the binary under both env names; off by default.
func TestShimPassesECSExportOptions(t *testing.T) {
	m := readManifest(t)
	if d, ok := m.UserConfig["ecs_export"]; !ok || d.Default != false {
		t.Fatalf("plugin.json needs an ecs_export option that defaults to off, got %+v", d)
	}
	if d, ok := m.UserConfig["ecs_export_path"]; !ok || d.Default != "" {
		t.Fatalf("plugin.json needs an ecs_export_path option that defaults to empty, got %+v", d)
	}
	data := t.TempDir()
	writeExe(t, filepath.Join(data, "bin", m.Version, "guardrails"), "#!/bin/sh\necho \"on=$GUARDRAILS_ECS_EXPORT/$CLAUDE_TOOLS_ECS_EXPORT path=$GUARDRAILS_ECS_EXPORT_PATH/$CLAUDE_TOOLS_ECS_EXPORT_PATH\"\n")
	env := map[string]string{"CLAUDE_PLUGIN_DATA": data}
	if _, out := runShim(t, env, "", "pre-tool-use"); out != "on=/ path=/\n" {
		t.Errorf("defaults: %q", out)
	}
	env["CLAUDE_PLUGIN_OPTION_ECS_EXPORT"] = "true"
	env["CLAUDE_PLUGIN_OPTION_ECS_EXPORT_PATH"] = "/var/log/g.jsonl"
	if _, out := runShim(t, env, "", "pre-tool-use"); out != "on=true/true path=/var/log/g.jsonl//var/log/g.jsonl\n" {
		t.Errorf("options: %q", out)
	}
	env = map[string]string{"CLAUDE_PLUGIN_DATA": data, "GUARDRAILS_ECS_EXPORT": "1"}
	if _, out := runShim(t, env, "", "pre-tool-use"); out != "on=1/1 path=/\n" {
		t.Errorf("env fallback: %q", out)
	}
}
