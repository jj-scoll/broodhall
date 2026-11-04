package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuardReport(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HANDOFF_STATE_DIR", dir)
	logGuardEvent("s1", "ask", "git-stash")
	logGuardEvent("s1", "ask", "git-stash")
	logGuardEvent("s2", "ask", "git-stash")
	logGuardEvent("s2", "would-ask", "git-reset-hard")
	logGuardEvent("s2", "would-block", "session-url")
	logGuardEvent("s2", "mask", "pii-wrap")
	os.WriteFile(filepath.Join(dir, "claude-handoff", "s1.events"+".off"), []byte("12"), 0o600) // not a ledger
	os.WriteFile(filepath.Join(dir, "claude-handoff", "junk.events"), []byte("garbage\n1\tmask\n"), 0o600)

	var out bytes.Buffer
	if code := guardReportRun(&out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := out.String()
	for _, want := range []string{"2 sessions", "git-stash", "pii-wrap", "git-reset-hard", "session-url", "Approved versus denied"} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
	// git-stash row: 3 asks, no would-counts.
	for _, line := range strings.Split(got, "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && f[0] == "git-stash" && (len(f) < 3 || f[2] != "3") {
			t.Errorf("git-stash row = %q, want 3 asks", line)
		}
	}
}

func TestGuardReportEmpty(t *testing.T) {
	t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
	var out bytes.Buffer
	if code := guardReportRun(&out); code != 0 || !strings.Contains(out.String(), "no guard events") {
		t.Errorf("exit %d, output %q", code, out.String())
	}
}
