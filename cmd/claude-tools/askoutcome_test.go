package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func askPre(t *testing.T, session, id, cmd string) string {
	t.Helper()
	in := []byte(fmt.Sprintf(`{"hook_event_name":"PreToolUse","session_id":%q,"tool_use_id":%q,"cwd":"/tmp","tool_name":"Bash","tool_input":{"command":%q}}`, session, id, cmd))
	var stdout, stderr bytes.Buffer
	preToolUseRun(in, "CT", func([]byte) ([]byte, []byte, int) { return nil, nil, 0 }, &stdout, &stderr)
	return stdout.String()
}

func askPost(event, session, id string, extra string) {
	askOutcomeRun([]byte(fmt.Sprintf(`{"hook_event_name":%q,"session_id":%q,"tool_use_id":%q,"tool_name":"Bash","tool_input":{"command":"x"},"tool_response":{"stdout":"ok"}%s}`, event, session, id, extra)))
}

func ledgerKinds(t *testing.T, session string) string {
	t.Helper()
	b, _ := os.ReadFile(guardLedger(session))
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if f := strings.Split(l, "\t"); len(f) == 3 {
			out = append(out, f[1]+" "+f[2])
		}
	}
	return strings.Join(out, ", ")
}

func TestAskThenPostToolUseIsApproved(t *testing.T) {
	t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
	if out := askPre(t, "s1", "toolu_01A", "git stash"); !strings.Contains(out, `"ask"`) {
		t.Fatalf("setup: git stash should ask, got %q", out)
	}
	askPost("PostToolUse", "s1", "toolu_01A", "")
	if got := ledgerKinds(t, "s1"); got != "ask git-stash, ran git-stash" {
		t.Errorf("ledger = %q", got)
	}
	// The pending ask is consumed: a repeat counts nothing.
	askPost("PostToolUse", "s1", "toolu_01A", "")
	if got := ledgerKinds(t, "s1"); got != "ask git-stash, ran git-stash" {
		t.Errorf("repeat changed the ledger: %q", got)
	}
}

func TestAskThenPostToolUseFailureIsApproved(t *testing.T) {
	t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
	askPre(t, "s1", "toolu_F", "git stash")
	askPost("PostToolUseFailure", "s1", "toolu_F", `,"error":"boom","is_interrupt":false`)
	if got := ledgerKinds(t, "s1"); got != "ask git-stash, ran git-stash" {
		t.Errorf("ledger = %q", got)
	}
}

func TestAskOutcomeIgnoresOtherSessionsAndIDs(t *testing.T) {
	t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
	askPre(t, "s1", "toolu_X", "git stash")
	askPost("PostToolUse", "s2", "toolu_X", "")    // same id, other session
	askPost("PostToolUse", "s1", "toolu_Y", "")    // same session, other id
	askPost("PostToolUse", "s1", "../toolu_X", "") // path-shaped id
	askPost("PostToolUse", "s1", "", "")
	if got := ledgerKinds(t, "s1"); got != "ask git-stash" {
		t.Errorf("s1 ledger = %q", got)
	}
	if got := ledgerKinds(t, "s2"); got != "" {
		t.Errorf("s2 ledger = %q", got)
	}
}

// A subagent shares the session id and has its own tool_use_ids; its ask resolves the same way.
func TestAskInSubagentIsApproved(t *testing.T) {
	t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
	in := []byte(`{"session_id":"s1","agent_id":"agent-7","tool_use_id":"toolu_sub","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"git stash"}}`)
	var o, e bytes.Buffer
	preToolUseRun(in, "CT", func([]byte) ([]byte, []byte, int) { return nil, nil, 0 }, &o, &e)
	askOutcomeRun([]byte(`{"hook_event_name":"PostToolUse","session_id":"s1","agent_id":"agent-7","tool_use_id":"toolu_sub","tool_name":"Bash"}`))
	if got := ledgerKinds(t, "s1"); got != "ask git-stash, ran git-stash" {
		t.Errorf("ledger = %q", got)
	}
}

func TestObserveWouldAskIsNotPending(t *testing.T) {
	t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
	t.Setenv("CLAUDE_TOOLS_OBSERVE", "true")
	if out := askPre(t, "s1", "toolu_O", "git stash"); strings.Contains(out, `"ask"`) {
		t.Skipf("git stash still asks in observe mode: %s", out)
	}
	askPost("PostToolUse", "s1", "toolu_O", "")
	got := ledgerKinds(t, "s1")
	if strings.Contains(got, "ran") || strings.Contains(got, "not-run") || !strings.Contains(got, "would-ask") {
		t.Errorf("ledger = %q, want a would-ask and no outcome", got)
	}
	if m, _ := filepath.Glob(filepath.Join(stateDir(), "*.ask.*")); len(m) != 0 {
		t.Errorf("observe mode left pending asks: %v", m)
	}
}

func TestAskWithoutIDLogsButCannotResolve(t *testing.T) {
	t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
	in := []byte(`{"session_id":"s1","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"git stash"}}`)
	var o, e bytes.Buffer
	preToolUseRun(in, "CT", func([]byte) ([]byte, []byte, int) { return nil, nil, 0 }, &o, &e)
	if got := ledgerKinds(t, "s1"); got != "ask git-stash" {
		t.Errorf("ledger = %q", got)
	}
	if m, _ := filepath.Glob(filepath.Join(stateDir(), "*.ask.*")); len(m) != 0 {
		t.Errorf("no id, no pending file: %v", m)
	}
}

func TestStopMarksPendingAsksNotRun(t *testing.T) {
	t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
	askPre(t, "s1", "toolu_1", "git stash")
	askPre(t, "s1", "toolu_2", "git stash")
	askPre(t, "s2", "toolu_3", "git stash")
	askPost("PostToolUse", "s1", "toolu_1", "")
	resolveAsks("s1", 0)
	if got := ledgerKinds(t, "s1"); got != "ask git-stash, ask git-stash, ran git-stash, not-run git-stash" {
		t.Errorf("s1 ledger = %q", got)
	}
	if got := ledgerKinds(t, "s2"); got != "ask git-stash" {
		t.Errorf("another session's ask must stay pending: %q", got)
	}
	resolveAsks("s1", 0)
	if n := strings.Count(ledgerKinds(t, "s1"), "not-run"); n != 1 {
		t.Errorf("not-run counted %d times", n)
	}
}

// Outcome lines must not inflate the receipt or statusline.
func TestOutcomeEventsStayOutOfReceipt(t *testing.T) {
	t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
	askPre(t, "s1", "toolu_1", "git stash")
	askPost("PostToolUse", "s1", "toolu_1", "")
	logGuardEvent("s1", "not-run", "x")
	evs, _ := newGuardEvents("s1")
	if got := eventList(evs); got != "ask git-stash" {
		t.Errorf("receipt events = %q", got)
	}
	if got := guardSummary("s1"); got != "guards 1 asked" {
		t.Errorf("summary = %q", got)
	}
}

func TestAskOutcomeNoPendingAskDoesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HANDOFF_STATE_DIR", dir)
	for _, in := range []string{``, `not json`, `{"session_id":"s1"}`, `{"session_id":"s1","tool_use_id":"toolu_1","hook_event_name":"PostToolUse"}`} {
		if askOutcomeRun([]byte(in)) != 0 {
			t.Errorf("%q: nonzero exit", in)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*", "*")); len(m) != 0 {
		t.Errorf("wrote files with no pending ask: %v", m)
	}
}

func TestAskOutcomeCorruptPendingFile(t *testing.T) {
	t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
	os.MkdirAll(stateDir(), 0o755)
	os.WriteFile(askFile("s1", "toolu_C"), []byte("\x00\xff garbage no tab"), 0o600)
	os.WriteFile(askFile("s1", "toolu_E"), nil, 0o600)
	askPost("PostToolUse", "s1", "toolu_C", "")
	askPost("PostToolUse", "s1", "toolu_E", "")
	// An unreadable rule still counts as an approved ask, under a placeholder rule.
	if got := ledgerKinds(t, "s1"); got != "ran unknown, ran unknown" {
		t.Errorf("ledger = %q", got)
	}
	os.Mkdir(askFile("s1", "toolu_D"), 0o755) // a directory where the file should be
	askPost("PostToolUse", "s1", "toolu_D", "")
	resolveAsks("s1", 0)
}

func TestReportShowsOutcomesAndHint(t *testing.T) {
	t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
	// 12 asks of rule easy: 11 ran, 1 not run. 10 asks of rule hard: 5 ran, 5 not run.
	for i := 0; i < 12; i++ {
		logGuardEvent("s1", "ask", "easy")
		if i < 11 {
			logGuardEvent("s1", "ran", "easy")
		} else {
			logGuardEvent("s1", "not-run", "easy")
		}
	}
	for i := 0; i < 10; i++ {
		logGuardEvent("s1", "ask", "hard")
		if i < 5 {
			logGuardEvent("s1", "ran", "hard")
		} else {
			logGuardEvent("s1", "not-run", "hard")
		}
	}
	// An old pending ask counts as not run at report time; a fresh one does not.
	logGuardEvent("s2", "ask", "old")
	logGuardEvent("s2", "ask", "fresh")
	recordAskPending("s2", "toolu_old", "old")
	recordAskPending("s2", "toolu_fresh", "fresh")
	old := time.Now().Add(-time.Hour)
	os.Chtimes(askFile("s2", "toolu_old"), old, old)

	var out bytes.Buffer
	guardReportRun(&out)
	got := out.String()
	cols := func(rule string) []string {
		for _, l := range strings.Split(got, "\n") {
			if f := strings.Fields(l); len(f) > 0 && f[0] == rule {
				return f
			}
		}
		t.Fatalf("no row for %s:\n%s", rule, got)
		return nil
	}
	if f := cols("easy"); f[len(f)-2] != "11" || f[len(f)-1] != "1" {
		t.Errorf("easy row = %v\n%s", f, got)
	}
	if f := cols("old"); f[len(f)-2] != "0" || f[len(f)-1] != "1" {
		t.Errorf("old row = %v", f)
	}
	if f := cols("fresh"); f[len(f)-2] != "0" || f[len(f)-1] != "0" {
		t.Errorf("fresh row = %v", f)
	}
	if !strings.Contains(got, "easy: consider relaxing") {
		t.Errorf("no relax hint for easy:\n%s", got)
	}
	if strings.Contains(got, "hard: consider") || strings.Contains(got, "old: consider") {
		t.Errorf("hint on a rule that is not nearly always approved:\n%s", got)
	}
	if !strings.Contains(got, "not run") {
		t.Errorf("report should explain not-run:\n%s", got)
	}
}

func TestReportNoHintBelowTenAsks(t *testing.T) {
	t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
	for i := 0; i < 9; i++ {
		logGuardEvent("s1", "ask", "few")
		logGuardEvent("s1", "ran", "few")
	}
	var out bytes.Buffer
	guardReportRun(&out)
	if strings.Contains(out.String(), "consider relaxing") {
		t.Errorf("hint with 9 asks:\n%s", out.String())
	}
}
