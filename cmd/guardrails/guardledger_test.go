package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func eventList(evs []guardEvent) string {
	var s []string
	for _, e := range evs {
		s = append(s, e.Kind+" "+e.Rule)
	}
	return strings.Join(s, ", ")
}

func TestGuardLedgerReadsOnlyNewEvents(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HANDOFF_STATE_DIR", dir)
	logGuardEvent("s1", "ask", "egress:pii")
	logGuardEvent("s1", "nudge", "read-dedup")
	logGuardEvent("s2", "block", "large-read")

	evs, commit := newGuardEvents("s1")
	if got := eventList(evs); got != "ask egress:pii, nudge read-dedup" {
		t.Fatalf("first read = %q", got)
	}
	if again, _ := newGuardEvents("s1"); len(again) != 2 {
		t.Errorf("uncommitted read must return the same events, got %q", eventList(again))
	}
	commit()
	logGuardEvent("s1", "mask", "pii-wrap")
	if evs, _ := newGuardEvents("s1"); eventList(evs) != "mask pii-wrap" {
		t.Errorf("after commit = %q, want only the new event", eventList(evs))
	}

	st, err := os.Stat(filepath.Join(dir, "claude-handoff", "s1.events"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("ledger mode = %v (%v), want 0600", st.Mode().Perm(), err)
	}
}

func TestGuardLedgerKeepsOnlyRuleIDs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HANDOFF_STATE_DIR", dir)
	logGuardEvent("s", "block", "dangerous:rm -rf /nohome/user")
	logGuardEvent("../escape", "ask", "x")
	logGuardEvent("", "ask", "x")
	if evs, _ := newGuardEvents("s"); eventList(evs) != "block dangerous:rm" {
		t.Errorf("events = %q, want the rule cut at its first space", eventList(evs))
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*")); len(m) != 1 {
		t.Errorf("unsafe session ids must write nothing, state dir holds %v", m)
	}
}

// Each guard branch that changes, stops or nudges a tool call leaves one ledger line.
func TestGuardsWriteLedger(t *testing.T) {
	type tc struct {
		name, want string
		run        func(t *testing.T)
	}
	bash := func(cmd string) func(t *testing.T) {
		return func(t *testing.T) { runPreHook(t, cmd, "") }
	}
	tool := func(fn func([]string) int, name string, input map[string]any) func(t *testing.T) {
		return func(t *testing.T) {
			b, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "session_id": "s", "cwd": t.TempDir(), "tool_name": name, "tool_input": input})
			runHook(t, fn, string(b))
		}
	}
	bigFile := func(t *testing.T, lines int) string {
		p := filepath.Join(t.TempDir(), "big.go")
		os.WriteFile(p, []byte(strings.Repeat("// a line of Go source code padded out to be long\n", lines)), 0o644)
		return p
	}
	tests := []tc{
		{"bash block", "block dangerous:git", bash("git push --force origin main")},
		{"bash ask", "ask git-reset-hard", bash("git reset --hard HEAD~3")},
		{"bash secret mask", "mask secret-mask", bash("cat ~/.config/mark.toml")},
		{"bash pii wrap", "mask pii-wrap", bash(`psql -t -A -c "select email from users"`)},
		{"bash nudge", "nudge slow-op", bash("docker build -t x .")},
		{"settings edit ask", "ask settings:Edit", func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			tool(preToolUse, "Edit", map[string]any{"file_path": filepath.Join(home, ".claude", "settings.json"), "old_string": "a", "new_string": "b"})(t)
		}},
		{"large read nudge", "nudge large-read", func(t *testing.T) {
			tool(largeReadGuard, "Read", map[string]any{"file_path": bigFile(t, 300)})(t)
		}},
		{"large read block", "block large-read", func(t *testing.T) {
			tool(largeReadGuard, "Read", map[string]any{"file_path": bigFile(t, 1500)})(t)
		}},
		{"read dedup nudge", "nudge read-dedup", func(t *testing.T) {
			session := "ledger-test-" + filepath.Base(t.TempDir())
			t.Cleanup(func() { os.Remove("/tmp/claude-readcache-" + session) })
			p := filepath.Join(t.TempDir(), "f.txt")
			os.WriteFile(p, []byte("x"), 0o644)
			b, _ := json.Marshal(map[string]any{"session_id": session, "tool_name": "Read", "tool_input": map[string]any{"file_path": p}})
			runHook(t, readDedup, string(b))
			runHook(t, readDedup, string(b))
			if evs, _ := newGuardEvents(session); eventList(evs) != "nudge read-dedup" {
				t.Errorf("events = %q, want nudge read-dedup", eventList(evs))
			}
		}},
		{"retry breaker nudge", "nudge retry-breaker", func(t *testing.T) {
			for i := 0; i < 3; i++ {
				hookEvent(t, "PostToolUseFailure", "s", "", "make test", `,"error":"Exit code 2\nboom","is_interrupt":false`)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
			tt.run(t)
			if tt.name == "read dedup nudge" {
				return // checked above: read-dedup keys the ledger by its own session id
			}
			if evs, _ := newGuardEvents("s"); eventList(evs) != tt.want {
				t.Errorf("events = %q, want %q", eventList(evs), tt.want)
			}
		})
	}
}

// A plain allowed command leaves no ledger line.
func TestGuardsQuietCommandWritesNoLedger(t *testing.T) {
	t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
	runPreHook(t, "ls -la", "")
	if evs, _ := newGuardEvents("s"); len(evs) != 0 {
		t.Errorf("events = %q, want none", eventList(evs))
	}
}

func TestSummaryAndReceiptCountObserved(t *testing.T) {
	t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
	logGuardEvent("w", "would-ask", "git-stash")
	logGuardEvent("w", "would-block", "session-url")
	if got := guardSummary("w"); got != "guards 1 would ask · 1 would block" {
		t.Errorf("guardSummary = %q", got)
	}
	evs, _ := newGuardEvents("w")
	if got := receiptLine(evs); got != "guards · would ask 1 (git-stash) · would block 1 (session-url)" {
		t.Errorf("receiptLine = %q", got)
	}
}
