package main

import (
	"strings"
	"testing"
)

// An ask rule answers through permissionDecision on stdout with exit 0: the user decides,
// and the reason reaches both of them.
func TestPreToolUseAsk(t *testing.T) {
	code, out, errOut := runHook(t, preToolUse, bashInput("PreToolUse", "s", "/w", "git reset --hard HEAD~3", ""))
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stderr=%q, want exit 0 and no stderr", code, errOut)
	}
	for _, want := range []string{`"hookEventName": "PreToolUse"`, `"permissionDecision": "ask"`, `"permissionDecisionReason": "ASK: `, "git reset --hard"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %s:\n%s", want, out)
		}
	}
}
