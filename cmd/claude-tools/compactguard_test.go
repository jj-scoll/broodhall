package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func compactPayload(session, trigger string) string {
	return fmt.Sprintf(`{"session_id":%q,"trigger":%q}`, session, trigger)
}

func TestCompactGuardBlocksFirstManualCompactAfterOne(t *testing.T) {
	gateEnv(t)
	// First compaction of the session, manual or auto, is never blocked.
	if code, _, _ := runHook(t, compactGuard, compactPayload("s1", "manual")); code != 0 {
		t.Fatalf("first manual compact: exit %d, want 0", code)
	}
	// An auto-compact after that is never blocked either.
	if code, _, _ := runHook(t, compactGuard, compactPayload("s1", "auto")); code != 0 {
		t.Fatalf("auto compact: exit %d, want 0", code)
	}
	// The next manual compact is blocked once, with a pointer at the handoff skill.
	code, _, stderr := runHook(t, compactGuard, compactPayload("s1", "manual"))
	if code != 2 {
		t.Fatalf("third compact: exit %d, want 2", code)
	}
	for _, want := range []string{"memory:handoff", "/clear", "/compact again"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q: %s", want, stderr)
		}
	}
	// Asking again goes through.
	if code, _, _ := runHook(t, compactGuard, compactPayload("s1", "manual")); code != 0 {
		t.Errorf("repeat manual compact: exit %d, want 0", code)
	}
	// Another session starts from zero.
	if code, _, _ := runHook(t, compactGuard, compactPayload("s2", "manual")); code != 0 {
		t.Errorf("fresh session: exit %d, want 0", code)
	}
}

func TestCompactGuardNeverBlocksAuto(t *testing.T) {
	gateEnv(t)
	for i := 0; i < 3; i++ {
		if code, _, _ := runHook(t, compactGuard, compactPayload("s", "auto")); code != 0 {
			t.Fatalf("auto compact %d: exit %d, want 0", i, code)
		}
	}
}

func TestCompactGuardRearmsTheGate(t *testing.T) {
	gateEnv(t)
	tr := writeTranscript(t, usageLine(250_000))
	if _, out, _ := runHook(t, gate, stopPayload("s", tr, false)); out == "" {
		t.Fatal("gate did not fire")
	}
	if _, out, _ := runHook(t, gate, stopPayload("s", tr, false)); out != "" {
		t.Fatal("gate fired twice before a compaction")
	}
	runHook(t, compactGuard, compactPayload("s", "auto"))
	if _, out, _ := runHook(t, gate, stopPayload("s", tr, false)); out == "" {
		t.Error("gate did not re-arm after a compaction")
	}
}

func TestCompactGuardIgnoresBadInput(t *testing.T) {
	gateEnv(t)
	for _, in := range []string{"not json", compactPayload("../evil", "manual"), compactPayload("", "manual")} {
		if code, _, _ := runHook(t, compactGuard, in); code != 0 {
			t.Errorf("input %q: exit %d, want 0", in, code)
		}
	}
	// Nothing was written for them.
	if entries, _ := os.ReadDir(filepath.Join(os.Getenv("HANDOFF_STATE_DIR"), "claude-handoff")); len(entries) != 0 {
		t.Errorf("state written for bad input: %v", entries)
	}
}

func TestCompactGuardCountFileTolerance(t *testing.T) {
	gateEnv(t)
	dir := filepath.Join(os.Getenv("HANDOFF_STATE_DIR"), "claude-handoff")
	os.MkdirAll(dir, 0o755)
	// A garbled counter reads as zero, so a manual compact is allowed.
	os.WriteFile(filepath.Join(dir, "s.compacts"), []byte("banana\n"), 0o644)
	if code, _, _ := runHook(t, compactGuard, compactPayload("s", "manual")); code != 0 {
		t.Errorf("garbled counter: exit %d, want 0", code)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "s.compacts")); string(b) != "1\n" {
		t.Errorf("counter = %q, want 1", b)
	}
}

func TestCompactGuardForgetsReadDedupLedger(t *testing.T) {
	gateEnv(t)
	session := "compact-readdedup-test"
	ledger := "/tmp/claude-readcache-" + session
	t.Cleanup(func() { os.Remove(ledger) })
	file := filepath.Join(t.TempDir(), "f.txt")
	os.WriteFile(file, []byte("hello\n"), 0o644)
	read := fmt.Sprintf(`{"session_id":%q,"tool_name":"Read","cwd":%q,"tool_input":{"file_path":%q}}`, session, filepath.Dir(file), file)

	runHook(t, readDedup, read)
	if _, out, _ := runHook(t, readDedup, read); !strings.Contains(out, "already read") {
		t.Fatalf("setup: second read gave no nudge: %q", out)
	}

	// A blocked manual compact does not compact, so the ledger stays.
	runHook(t, compactGuard, compactPayload(session, "auto"))
	runHook(t, readDedup, read)
	if code, _, _ := runHook(t, compactGuard, compactPayload(session, "manual")); code != 2 {
		t.Fatalf("manual compact: exit %d, want 2", code)
	}
	if _, err := os.Stat(ledger); err != nil {
		t.Error("blocked compact removed the read ledger")
	}

	// A compaction that goes through leaves the conversation without the earlier read.
	runHook(t, compactGuard, compactPayload(session, "auto"))
	runHook(t, readDedup, read)
	if _, out, _ := runHook(t, readDedup, read); !strings.Contains(out, "already read") {
		t.Fatalf("read after compaction should record again, then nudge: %q", out)
	}
	runHook(t, compactGuard, compactPayload(session, "auto"))
	if _, out, _ := runHook(t, readDedup, read); strings.Contains(out, "already read") {
		t.Errorf("nudge after compaction: %q", out)
	}
}

func TestReadDedupSkipsUnsafeSessionIDs(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f.txt")
	os.WriteFile(file, []byte("hello\n"), 0o644)
	tests := []struct{ name, session, ledger string }{
		{"slash", "../tmp/rd-x", "/tmp/claude-readcache-../tmp/rd-x"},
		{"dotdot", "..", "/tmp/claude-readcache-.."},
		{"underscore", "ver_under_1", "/tmp/claude-readcache-ver_under_1"},
		{"missing", "", "/tmp/claude-readcache-nosession"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Remove(tt.ledger)
			t.Cleanup(func() { os.Remove(tt.ledger) })
			sid := ""
			if tt.session != "" {
				sid = fmt.Sprintf(`"session_id":%q,`, tt.session)
			}
			read := fmt.Sprintf(`{%s"tool_name":"Read","cwd":%q,"tool_input":{"file_path":%q}}`, sid, filepath.Dir(file), file)
			for i := 0; i < 2; i++ {
				if code, out, _ := runHook(t, readDedup, read); code != 0 || out != "" {
					t.Fatalf("read %d: exit %d, output %q; want silent", i, code, out)
				}
			}
			if _, err := os.Stat(tt.ledger); err == nil {
				t.Errorf("ledger %s was written", tt.ledger)
			}
		})
	}
}

func TestReadDedupUUIDSessionClearedByCompaction(t *testing.T) {
	gateEnv(t)
	session := "7c04244b-7e74-4b7f-91cd-632add8d565b"
	t.Cleanup(func() { os.Remove(readCachePath(session)) })
	file := filepath.Join(t.TempDir(), "f.txt")
	os.WriteFile(file, []byte("hello\n"), 0o644)
	read := fmt.Sprintf(`{"session_id":%q,"tool_name":"Read","cwd":%q,"tool_input":{"file_path":%q}}`, session, filepath.Dir(file), file)
	runHook(t, readDedup, read)
	if _, out, _ := runHook(t, readDedup, read); !strings.Contains(out, "already read") {
		t.Fatalf("no nudge for a UUID session: %q", out)
	}
	runHook(t, compactGuard, compactPayload(session, "auto"))
	if _, out, _ := runHook(t, readDedup, read); out != "" {
		t.Errorf("nudge after compaction: %q", out)
	}
}
