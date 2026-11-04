package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// CLAUDE_TOOLS_OFF turns whole rule families off. The guardrails plugin sets it from its
// disabled_guards option, so rules built around one person's data stay off for everyone else.
func TestGuardsOff(t *testing.T) {
	saved := egressOwn
	egressOwn = func() ([]string, string) { return []string{"me@example.com"}, "example.com" }
	defer func() { egressOwn = saved }()
	withClientBuckets(t, testClientBuckets)
	tests := []struct{ family, cmd, on string }{
		{"raw-client-data", `uv run python q.py s3://acme-uat-clientdata/exports/`, "ask:raw-client-data"},
		{"pii", `duckdb -cmd '.mode line' -c "from people"`, "block:duckdb-dot-command"},
		{"session-url", `gh pr create --title t --body "fixes it ` + fakeSessionURL + `"`, "block:session-url"},
		{"egress", `gh pr comment 1 -b "cc alex@other.org"`, "ask:egress:email"},
		{"git", `git reset --hard`, "ask:git-reset-hard"},
	}
	for _, tt := range tests {
		env := envAt(memState{}, t0, "/w")
		if got := decide(env, tt.cmd).Kind(); got != tt.on {
			t.Errorf("%s on: %q is %s, want %s", tt.family, tt.cmd, got, tt.on)
		}
		env.Off = guardsOff(" " + tt.family + " ,unknown")
		if got := decide(env, tt.cmd).Kind(); got != "allow" {
			t.Errorf("%s off: %q is %s, want allow", tt.family, tt.cmd, got)
		}
	}
	// The core blocks have no switch.
	env := envAt(memState{}, t0, "/w")
	env.Off = guardsOff("pii,raw-client-data,session-url,egress,git")
	if got := decide(env, "rm -rf ~").Kind(); got != "block:dangerous:rm -rf ~" {
		t.Errorf("rm -rf ~ with every family off is %s", got)
	}
}

// The hook reads CLAUDE_TOOLS_OFF: pii off leaves SQL clients unwrapped, egress off lets an
// MCP send through.
func TestGuardsOffHook(t *testing.T) {
	sql := `psql -t -A -c "select email from users"`
	t.Setenv("CLAUDE_TOOLS_OFF", "")
	if _, out, _ := runPreHook(t, sql, ""); !strings.Contains(out, "redact --pii") {
		t.Fatalf("pii on: want the redact wrap, got %q", out)
	}
	t.Setenv("CLAUDE_TOOLS_OFF", "pii")
	if _, out, _ := runPreHook(t, sql, ""); strings.Contains(out, "redact") {
		t.Fatalf("pii off: want no wrap, got %q", out)
	}
	// The new name wins over the old one, even set empty (every family on).
	t.Setenv("GUARDRAILS_OFF", "")
	if _, out, _ := runPreHook(t, sql, ""); !strings.Contains(out, "redact --pii") {
		t.Fatalf("GUARDRAILS_OFF empty beats CLAUDE_TOOLS_OFF=pii: want the wrap, got %q", out)
	}
	t.Setenv("GUARDRAILS_OFF", "pii")
	t.Setenv("CLAUDE_TOOLS_OFF", "")
	if _, out, _ := runPreHook(t, sql, ""); strings.Contains(out, "redact") {
		t.Fatalf("GUARDRAILS_OFF=pii: want no wrap, got %q", out)
	}
	os.Unsetenv("GUARDRAILS_OFF")

	saved := egressOwn
	egressOwn = func() ([]string, string) { return []string{"me@example.com"}, "example.com" }
	defer func() { egressOwn = saved }()
	in, _ := json.Marshal(map[string]any{"session_id": "s", "cwd": t.TempDir(), "tool_name": "mcp__claude_ai_Gmail__send_message",
		"tool_input": map[string]any{"to": "jane@acme-client.com", "body": "hi"}})
	send := func() string {
		var stdout, stderr bytes.Buffer
		preToolUseRun(in, "CT", func([]byte) ([]byte, []byte, int) { return nil, nil, 0 }, &stdout, &stderr)
		return stdout.String()
	}
	t.Setenv("CLAUDE_TOOLS_OFF", "")
	if out := send(); !strings.Contains(out, `"ask"`) {
		t.Fatalf("egress on: want ask, got %q", out)
	}
	t.Setenv("CLAUDE_TOOLS_OFF", "egress")
	if out := send(); strings.Contains(out, `"ask"`) {
		t.Fatalf("egress off: want allow, got %q", out)
	}
}
