package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// The tests that run preToolUse whole must not depend on the rtk installed on this machine.
func init() { rtkHook = func([]byte) ([]byte, []byte, int) { return nil, nil, 0 } }

type hookReply struct {
	HookSpecificOutput struct {
		PermissionDecision string         `json:"permissionDecision"`
		UpdatedInput       map[string]any `json:"updatedInput"`
		AdditionalContext  string         `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

// runPreHook feeds one Bash PreToolUse input to the hook with a fake rtk and returns exit,
// stdout, and the stdin rtk saw (nil when rtk was not run).
func runPreHook(t *testing.T, cmd string, rtkOut string) (int, string, []byte) {
	t.Helper()
	in, _ := json.Marshal(map[string]any{"session_id": "s", "cwd": t.TempDir(), "tool_name": "Bash",
		"tool_input": map[string]any{"command": cmd, "description": "d", "timeout": 5000}})
	var rtkSaw []byte
	rtk := func(b []byte) ([]byte, []byte, int) {
		rtkSaw = b
		if msg, ok := strings.CutPrefix(rtkOut, "EXIT2:"); ok {
			return nil, []byte(msg), 2
		}
		return []byte(rtkOut), nil, 0
	}
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := preToolUseRun(in, "CT", rtk, &stdout, &stderr)
	return code, stdout.String(), rtkSaw
}

func TestPreToolUseWrapsSQLWithoutRTK(t *testing.T) {
	code, out, rtkSaw := runPreHook(t, `psql -t -A -c "select email from users"`, `{"hookSpecificOutput":{"updatedInput":{"command":"rtk psql"}}}`)
	if code != 0 || rtkSaw != nil {
		t.Fatalf("exit %d, rtk ran=%v", code, rtkSaw != nil)
	}
	var r hookReply
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("bad reply %q: %v", out, err)
	}
	ui := r.HookSpecificOutput.UpdatedInput
	if ui["command"] != `CT redact --pii --private --in=html --to=unaligned --no-header -- psql --html -c "select email from users"` {
		t.Errorf("command = %v", ui["command"])
	}
	if ui["description"] != "d" || ui["timeout"] != float64(5000) {
		t.Errorf("other tool_input fields lost: %v", ui)
	}
	if r.HookSpecificOutput.PermissionDecision != "" {
		t.Errorf("SQL rewrite must leave approval to the user's rules, got %q", r.HookSpecificOutput.PermissionDecision)
	}
}

func TestPreToolUsePassesRTKThrough(t *testing.T) {
	const rtkReply = `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecisionReason":"RTK auto-rewrite","updatedInput":{"command":"rtk git status"},"permissionDecision":"allow"}}`
	code, out, rtkSaw := runPreHook(t, "git status", rtkReply)
	if code != 0 || !strings.Contains(string(rtkSaw), `"command":"git status"`) {
		t.Fatalf("exit %d, rtk saw %s", code, rtkSaw)
	}
	if strings.TrimSpace(out) != rtkReply {
		t.Errorf("rtk reply changed:\n got %s\nwant %s", out, rtkReply)
	}
	if _, out, _ := runPreHook(t, "echo hi", ""); out != "" {
		t.Errorf("no rtk rewrite should print nothing, got %q", out)
	}
}

func TestPreToolUseMergesContextIntoRTK(t *testing.T) {
	_, out, _ := runPreHook(t, "terraform apply", `{"hookSpecificOutput":{"hookEventName":"PreToolUse","updatedInput":{"command":"rtk terraform apply"}}}`)
	var r hookReply
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("bad reply %q: %v", out, err)
	}
	if r.HookSpecificOutput.UpdatedInput["command"] != "rtk terraform apply" || !strings.Contains(r.HookSpecificOutput.AdditionalContext, "plan") {
		t.Errorf("want rtk rewrite plus the plan-before-apply nudge, got %s", out)
	}
}

func TestPreToolUseBlockAndAskSkipRTK(t *testing.T) {
	if code, _, rtkSaw := runPreHook(t, "git push --force origin main", `{"x":1}`); code != 2 || rtkSaw != nil {
		t.Errorf("force push: exit %d, rtk ran=%v", code, rtkSaw != nil)
	}
	code, out, rtkSaw := runPreHook(t, "git branch -D old", `{"x":1}`)
	if code != 0 || rtkSaw != nil || !strings.Contains(out, `"permissionDecision": "ask"`) {
		t.Errorf("branch -D: exit %d, rtk ran=%v, out %s", code, rtkSaw != nil, out)
	}
}

func TestPreToolUseForwardsRTKBlock(t *testing.T) {
	if code, _, _ := runPreHook(t, "git status", "EXIT2:rtk pipeline rule denied this"); code != 2 {
		t.Errorf("rtk exit 2 must block, got exit %d", code)
	}
}
