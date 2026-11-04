package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// usageLine is one transcript line: an assistant message whose context is `tokens` (split
// over the three input counters the way the API reports them).
func usageLine(tokens int) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"usage":{"input_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,"output_tokens":50}}}`,
		1, tokens-101, 100)
}

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func stopPayload(session, transcript string, active any) string {
	b, _ := json.Marshal(map[string]any{
		"session_id": session, "transcript_path": transcript, "cwd": "/work", "stop_hook_active": active,
	})
	return string(b)
}

// gateEnv gives one test its own state dir and a clean threshold environment.
func gateEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
	t.Setenv("HANDOFF_TOKENS", "")
	t.Setenv("CLAUDE_CODE_AUTO_COMPACT_WINDOW", "")
}

func TestGateBlocksOnceAtThreshold(t *testing.T) {
	gateEnv(t)
	tr := writeTranscript(t, usageLine(250_000))
	code, out, _ := runHook(t, gate, stopPayload("sess-1", tr, false))
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	var reply struct{ Decision, Reason string }
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		t.Fatalf("stdout is not JSON: %q", out)
	}
	if reply.Decision != "block" {
		t.Errorf("decision %q, want block", reply.Decision)
	}
	for _, want := range []string{"250000 tokens", "threshold 240000", "memory:handoff"} {
		if !strings.Contains(reply.Reason, want) {
			t.Errorf("reason lacks %q: %s", want, reply.Reason)
		}
	}
	// Once per session: the same session at the same size passes.
	if _, out, _ := runHook(t, gate, stopPayload("sess-1", tr, false)); out != "" {
		t.Errorf("second stop blocked again: %s", out)
	}
	// Another session is gated on its own.
	if _, out, _ := runHook(t, gate, stopPayload("sess-2", tr, false)); out == "" {
		t.Error("a different session was not blocked")
	}
}

func TestGateSilentCases(t *testing.T) {
	big := writeTranscript(t, usageLine(250_000))
	small := writeTranscript(t, usageLine(100_000))
	noUsage := writeTranscript(t, `{"type":"user","message":{"content":"hi"}}`)
	tests := []struct {
		name, input string
	}{
		{"below threshold", stopPayload("s", small, false)},
		{"stop hook already active", stopPayload("s", big, true)},
		{"stop hook active as string", stopPayload("s", big, "true")},
		{"no usage in transcript", stopPayload("s", noUsage, false)},
		{"missing transcript", stopPayload("s", filepath.Join(t.TempDir(), "none.jsonl"), false)},
		{"unsafe session id", stopPayload("../x", big, false)},
		{"not json", "not json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gateEnv(t)
			code, out, _ := runHook(t, gate, tt.input)
			if code != 0 || out != "" {
				t.Errorf("exit %d, stdout %q; want silent exit 0", code, out)
			}
		})
	}
}

func TestGateThreshold(t *testing.T) {
	tests := []struct {
		name, tokensEnv, windowEnv string
		context                    int
		wantBlock                  bool
	}{
		{"default is 80% of a 300k window", "", "", 239_999, false},
		{"default window at the line", "", "", 240_000, true},
		{"window env scales it", "", "100000", 80_000, true},
		{"window env, below", "", "100000", 79_999, false},
		{"HANDOFF_TOKENS wins over the window", "50000", "1000000", 50_000, true},
		{"HANDOFF_TOKENS, below", "50000", "", 49_999, false},
		{"junk HANDOFF_TOKENS falls back", "lots", "", 239_999, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gateEnv(t)
			t.Setenv("HANDOFF_TOKENS", tt.tokensEnv)
			t.Setenv("CLAUDE_CODE_AUTO_COMPACT_WINDOW", tt.windowEnv)
			tr := writeTranscript(t, usageLine(tt.context))
			_, out, _ := runHook(t, gate, stopPayload("s", tr, false))
			if got := out != ""; got != tt.wantBlock {
				t.Errorf("blocked = %v, want %v", got, tt.wantBlock)
			}
		})
	}
}

// The latest assistant usage is the context size: a session that compacted is small again.
func TestGateReadsLatestUsage(t *testing.T) {
	gateEnv(t)
	tr := writeTranscript(t, usageLine(250_000), `not json at all`, usageLine(30_000))
	if _, out, _ := runHook(t, gate, stopPayload("s", tr, false)); out != "" {
		t.Errorf("blocked on a stale usage line: %s", out)
	}
}

// Only the tail of a large transcript is read; the partial first line there is skipped.
func TestGateReadsOnlyTheTail(t *testing.T) {
	gateEnv(t)
	filler := `{"type":"user","message":{"content":"` + strings.Repeat("x", 1000) + `"}}`
	lines := []string{usageLine(10_000)}
	for i := 0; i < 5000; i++ {
		lines = append(lines, filler)
	}
	lines = append(lines, usageLine(250_000))
	tr := writeTranscript(t, lines...)
	if _, out, _ := runHook(t, gate, stopPayload("s", tr, false)); out == "" {
		t.Error("did not find the usage line at the end of a 5MB transcript")
	}
}

func TestGateMessageNamesOnlyPublicTools(t *testing.T) {
	gateEnv(t)
	tr := writeTranscript(t, usageLine(250_000))
	_, out, _ := runHook(t, gate, stopPayload("s", tr, false))
	for _, bad := range []string{"handoff_writer", ".handoff-next", "write-handoff"} {
		if strings.Contains(out, bad) {
			t.Errorf("gate message mentions %q", bad)
		}
	}
}
