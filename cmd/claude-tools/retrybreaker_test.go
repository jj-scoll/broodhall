package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetryObserveIdentical(t *testing.T) {
	fail := func(s *retryState, cmd string) string { return s.observe(cmd, true) }

	t.Run("third identical failure nudges once", func(t *testing.T) {
		s := &retryState{}
		for i, want := range []bool{false, false, true, false} {
			got := fail(s, "make test")
			s.observe("echo ok", false)
			if (got != "") != want {
				t.Errorf("failure %d: nudge=%q, want nudge=%v", i+1, got, want)
			}
		}
	})
	t.Run("consecutive failures of different commands never nudge", func(t *testing.T) {
		s := &retryState{}
		for i, c := range []string{"a1", "b2", "c3", "d4", "e5", "f6", "g7", "h8"} {
			if got := fail(s, c); got != "" {
				t.Errorf("failure %d: nudge=%q, want none (probing, not retrying)", i+1, got)
			}
		}
	})
	t.Run("a success resets that command's count", func(t *testing.T) {
		s := &retryState{}
		fail(s, "make test")
		fail(s, "make test")
		s.observe("make test", false) // it finally passed
		if got := fail(s, "make test"); got != "" {
			t.Errorf("count should reset when the command succeeds, got %q", got)
		}
	})
	t.Run("whitespace differences are the same command", func(t *testing.T) {
		s := &retryState{}
		fail(s, "make   test")
		fail(s, "make test ")
		if got := fail(s, "make\ttest"); got == "" {
			t.Error("normalized commands should count together")
		}
	})
	t.Run("message quotes the exploration budget", func(t *testing.T) {
		s := &retryState{}
		var msg string
		for i := 0; i < 3; i++ {
			msg = fail(s, "same")
		}
		for _, want := range []string{"3", "stop", "change"} {
			if !strings.Contains(strings.ToLower(msg), want) {
				t.Errorf("message %q lacks %q", msg, want)
			}
		}
	})
}

func hookEvent(t *testing.T, event, session, agent, cmd string, extra string) (int, string, string) {
	t.Helper()
	if agent != "" {
		extra += `,"agent_id":"` + agent + `"`
	}
	return runHook(t, retryBreaker, bashInput(event, session, "/w", cmd, extra))
}

func TestRetryBreakerHook(t *testing.T) {
	t.Setenv("HANDOFF_STATE_DIR", t.TempDir())

	for i := 1; i <= 3; i++ {
		code, out, errOut := hookEvent(t, "PostToolUseFailure", "sess", "", "make test", `,"error":"Exit code 2\nboom","is_interrupt":false`)
		if code != 0 || errOut != "" {
			t.Fatalf("failure %d: exit %d stderr %q", i, code, errOut)
		}
		if i < 3 {
			if out != "" {
				t.Fatalf("failure %d: unexpected output %q", i, out)
			}
			continue
		}
		var got struct {
			HookSpecificOutput map[string]any `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("stdout not JSON: %v\n%s", err, out)
		}
		h := got.HookSpecificOutput
		if h["hookEventName"] != "PostToolUseFailure" {
			t.Errorf("hookEventName = %v, want PostToolUseFailure", h["hookEventName"])
		}
		if s, _ := h["additionalContext"].(string); !strings.Contains(s, "3") {
			t.Errorf("additionalContext = %q", s)
		}
	}
}

func TestRetryBreakerIgnoresInterruptsAndOtherTools(t *testing.T) {
	t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
	for i := 0; i < 6; i++ {
		_, out, _ := hookEvent(t, "PostToolUseFailure", "sess", "", "sleep 100", `,"error":"aborted","is_interrupt":true`)
		if out != "" {
			t.Fatalf("interrupt %d produced output %q", i, out)
		}
	}
	in := `{"hook_event_name":"PostToolUseFailure","session_id":"sess","tool_name":"Read","tool_input":{"file_path":"/x"},"error":"nope"}`
	for i := 0; i < 6; i++ {
		if _, out, _ := runHook(t, retryBreaker, in); out != "" {
			t.Fatalf("non-Bash failure %d produced output %q", i, out)
		}
	}
}

func TestRetryBreakerSeparatesSubagentsAndSessions(t *testing.T) {
	t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
	fail := func(session, agent string) string {
		_, out, _ := hookEvent(t, "PostToolUseFailure", session, agent, "make test", `,"error":"Exit code 1"`)
		return out
	}
	fail("s", "")
	fail("s", "agentA")
	fail("s2", "")
	// Three failures of the same command, but split over main, agentA and another session.
	if out := fail("s", "agentB"); out != "" {
		t.Errorf("failures from different agents/sessions must not add up: %q", out)
	}
}

func TestRetryBreakerSuccessWithoutStateWritesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HANDOFF_STATE_DIR", dir)
	if code, out, _ := hookEvent(t, "PostToolUse", "sess", "", "ls", `,"tool_response":{"stdout":"x"}`); code != 0 || out != "" {
		t.Fatalf("exit %d out %q", code, out)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "claude-handoff", "*")); len(m) != 0 {
		t.Errorf("success with no prior failure created state: %v", m)
	}
}

func TestRetryBreakerSuccessResetsPersistedCount(t *testing.T) {
	t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
	for i := 0; i < 2; i++ {
		hookEvent(t, "PostToolUseFailure", "sess", "", "make test", `,"error":"Exit code 1"`)
	}
	hookEvent(t, "PostToolUse", "sess", "", "make test", `,"tool_response":{"stdout":"x"}`)
	if _, out, _ := hookEvent(t, "PostToolUseFailure", "sess", "", "make test", `,"error":"Exit code 1"`); out != "" {
		t.Errorf("count survived a success: %q", out)
	}
}

func TestRetryBreakerUnsafeSessionIsNoop(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HANDOFF_STATE_DIR", dir)
	for i := 0; i < 5; i++ {
		if code, out, _ := hookEvent(t, "PostToolUseFailure", "../evil", "", "x", `,"error":"Exit code 1"`); code != 0 || out != "" {
			t.Fatalf("exit %d out %q", code, out)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "evil.retry")); err == nil {
		t.Error("wrote outside the state dir")
	}
}
