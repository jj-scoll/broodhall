package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

// memdFixture reads the synthetic transcript; @@SECRET@@ becomes a fake token built in
// memdredact_test.go so no secret-shaped literal sits in the fixture file.
func memdFixture(t *testing.T) [][]byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/memory/transcript_basic.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.ReplaceAll(raw, []byte("@@SECRET@@"), []byte(memdFakeGH))
	return bytes.Split(bytes.TrimRight(raw, "\n"), []byte("\n"))
}

func kinds(evs []memdEvent) string {
	var k []string
	for _, e := range evs {
		k = append(k, e.Kind)
	}
	return strings.Join(k, ",")
}

func TestMemdTranscriptBasic(t *testing.T) {
	lines := memdFixture(t)
	const path = "/home/u/.claude/projects/-work-demo/sess-basic-0001.jsonl"

	// Batch 1: prompt, two tool_uses, a malformed line. Neither tool_use has a result yet.
	st := &memdFileState{}
	evs := parseMemdLines(path, "acct", lines[:4], st)
	pending := st.Pending
	if got := kinds(evs); got != "prompt" {
		t.Fatalf("batch1 kinds = %q, want prompt", got)
	}
	p := evs[0]
	if p.PromptText != "fix the flaky test in parser" || p.SessionID != "sess-basic-0001" || p.Cwd != "/work/demo" ||
		p.Account != "acct" || p.Project != projectKey("/work/demo") || p.Observer || p.Sub || p.At.IsZero() {
		t.Errorf("prompt event = %+v", p)
	}
	if len(pending) != 2 || pending["toolu_01"] == nil || pending["toolu_02"] == nil {
		t.Fatalf("pending = %v, want toolu_01 and toolu_02", pending)
	}

	// Batch 2: first result and the end_turn.
	evs = parseMemdLines(path, "acct", lines[4:6], st)
	pending = st.Pending
	if got := kinds(evs); got != "tool_use,turn_end" {
		t.Fatalf("batch2 kinds = %q", got)
	}
	tu := evs[0]
	if want := time.Date(2026, 10, 28, 10, 0, 1, 0, time.UTC); !tu.At.Equal(want) {
		t.Errorf("tool_use At = %v, want the tool_use line time %v", tu.At, want)
	}
	if tu.ToolUseID != "toolu_01" || tu.ToolName != "Bash" {
		t.Errorf("tool_use = %+v", tu)
	}
	if strings.Contains(tu.Input, memdFakeGH) || strings.Contains(tu.Output, memdFakeGH) {
		t.Errorf("secret survived:\nin=%q\nout=%q", tu.Input, tu.Output)
	}
	if !strings.Contains(tu.Input, `"command":"curl -H`) { // cmdNorm dropped the rtk prefix
		t.Errorf("command not normalized: %q", tu.Input)
	}
	if !strings.Contains(tu.Output, "HTTP 200") {
		t.Errorf("output = %q", tu.Output)
	}
	if _, still := pending["toolu_01"]; still || len(pending) != 1 {
		t.Errorf("pending after result = %v", pending)
	}

	// Batch 3: the later result for toolu_02, with block-list content.
	evs = parseMemdLines(path, "acct", lines[6:], st)
	pending = st.Pending
	if len(evs) != 1 || evs[0].ToolUseID != "toolu_02" || evs[0].Output != "package parser" || len(pending) != 0 {
		t.Errorf("batch3 = %+v pending=%v", evs, pending)
	}
}

func TestMemdTranscriptFlags(t *testing.T) {
	lines := memdFixture(t)
	tests := []struct {
		name, path string
		mutate     func(string) string
		observer   bool
		sub        bool
	}{
		{"plain", "/h/.claude/projects/-work-demo/s.jsonl", nil, false, false},
		{"memory observer cwd path", "/h/.claude/projects/-home-u--memory-observer-cwd/s.jsonl", nil, true, false},
		{"sdk entrypoint", "/h/.claude/projects/-work-demo/s.jsonl",
			func(s string) string { return strings.ReplaceAll(s, `"entrypoint":"cli"`, `"entrypoint":"sdk-cli"`) }, true, false},
		{"subagents path", "/h/.claude/projects/-work-demo/s/subagents/agent-1.jsonl", nil, false, true},
		{"sidechain line", "/h/.claude/projects/-work-demo/s.jsonl",
			func(s string) string { return strings.ReplaceAll(s, `"isSidechain":false`, `"isSidechain":true`) }, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := lines
			if tt.mutate != nil {
				in = nil
				for _, l := range lines {
					in = append(in, []byte(tt.mutate(string(l))))
				}
			}
			evs := parseMemdLines(tt.path, "a", in, nil)
			// Events follow the line that completes them: result 01, end_turn, result 02.
			if got := kinds(evs); got != "prompt,tool_use,turn_end,tool_use" {
				t.Fatalf("kinds = %q", got)
			}
			for _, e := range evs {
				if e.Observer != tt.observer || e.Sub != tt.sub {
					t.Errorf("%s: Observer=%v Sub=%v, want %v %v", e.Kind, e.Observer, e.Sub, tt.observer, tt.sub)
				}
			}
		})
	}
}

func TestMemdTranscriptPromptRules(t *testing.T) {
	tests := []struct {
		name, line string
		want       int
	}{
		{"tool_result only is not a prompt", `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"r"}]}}`, 0},
		{"meta is not a prompt", `{"type":"user","isMeta":true,"message":{"role":"user","content":"hi"}}`, 0},
		{"typed text blocks", `{"type":"user","timestamp":"2026-10-28T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`, 1},
		{"not json", `nope`, 0},
		{"json non-object", `[1,2]`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evs := parseMemdLines("/p/s.jsonl", "a", [][]byte{[]byte(tt.line)}, nil)
			if len(evs) != tt.want {
				t.Errorf("events = %+v, want %d", evs, tt.want)
			}
		})
	}
}

// Observer and Sub latch per file: a signal on a later line, in a later call, applies to a
// tool_use parsed before it, and to batches that carry no signal at all.
func TestMemdTranscriptLatch(t *testing.T) {
	use := func(ep string, side bool) []byte {
		return []byte(`{"type":"assistant","sessionId":"s","cwd":"/w","entrypoint":"` + ep + `","isSidechain":` +
			map[bool]string{true: "true", false: "false"}[side] + `,"timestamp":"2026-10-28T10:00:00Z","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]}}`)
	}
	result := func(id, extra string) []byte {
		return []byte(`{"type":"user","sessionId":"s","cwd":"/w",` + extra + `"timestamp":"2026-10-28T10:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"` + id + `","content":"ok"}]}}`)
	}
	tests := []struct {
		name             string
		first, second    []byte
		wantObs, wantSub bool
	}{
		{"sdk on the result line", use("cli", false), result("t1", `"entrypoint":"sdk-cli",`), true, false},
		{"sidechain on the result line", use("cli", false), result("t1", `"isSidechain":true,`), false, true},
		{"signal on first line, none later", use("sdk-cli", true), result("t1", ``), true, true},
		{"no signal anywhere", use("cli", false), result("t1", ``), false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &memdFileState{}
			if evs := parseMemdLines("/p/s.jsonl", "a", [][]byte{tt.first}, st); len(evs) != 0 {
				t.Fatalf("unexpected events %+v", evs)
			}
			evs := parseMemdLines("/p/s.jsonl", "a", [][]byte{tt.second}, st)
			if len(evs) != 1 || evs[0].Observer != tt.wantObs || evs[0].Sub != tt.wantSub {
				t.Fatalf("got %+v, want Observer=%v Sub=%v", evs, tt.wantObs, tt.wantSub)
			}
			// A later batch with no signal fields keeps the latched values.
			evs = parseMemdLines("/p/s.jsonl", "a", [][]byte{[]byte(`{"type":"assistant","message":{"stop_reason":"end_turn","content":[]}}`)}, st)
			if len(evs) != 1 || evs[0].Observer != tt.wantObs || evs[0].Sub != tt.wantSub {
				t.Fatalf("later batch: got %+v, want Observer=%v Sub=%v", evs, tt.wantObs, tt.wantSub)
			}
		})
	}
}
