package main

import (
	"strings"
	"testing"
)

// End to end: what the tailer stores, queues and hands the observer.
func TestCaptureStoredAndObserved(t *testing.T) {
	old := captureCfg
	defer func() { captureCfg = old }()
	captureCfg = captureSettings{Tools: true, Home: capHome}

	use := func(id, name, input string) string {
		return `{"type":"assistant","sessionId":"sess-c","cwd":"/work/demo","entrypoint":"cli","timestamp":"2026-11-01T10:00:00Z","message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"` + id + `","name":"` + name + `","input":` + input + `}]}}` + "\n"
	}
	res := func(id, text string) string {
		return `{"type":"user","sessionId":"sess-c","cwd":"/work/demo","entrypoint":"cli","timestamp":"2026-11-01T10:00:01Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id + `","content":` + jsonStr(text) + `}]}}` + "\n"
	}
	body := use("t1", "Read", `{"file_path":"/work/demo/.env"}`) + res("t1", "DB_PASSWORD=sekritsekrit") +
		use("t2", "Bash", `{"command":"gh auth token"}`) + res("t2", "tokentokentoken") +
		use("t3", "Read", `{"file_path":"/work/demo/a.go"}`) + res("t3", "package a")

	e := newTailEnv(t, "")
	tailAppend(t, e.path("sess-c.jsonl"), body)
	tailScan(t, e.tailer())
	if got := tailCount(t, e.db, "SELECT count(*) FROM queue"); got != 3 {
		t.Fatalf("queue = %d, want 3 (output-dropped calls are still observed)", got)
	}
	for _, id := range []string{"t1", "t2"} {
		var out string
		if err := e.db.QueryRow(`SELECT output FROM tool_uses WHERE tool_use_id = ?`, id).Scan(&out); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(out, "[output not kept") {
			t.Errorf("%s stored output = %q", id, out)
		}
		it := &queueItem{ToolName: "Read", Input: "{}", Output: out}
		if p, _ := observerPayload(it); !strings.Contains(p, "[output not kept") || strings.Contains(p, "sekrit") {
			t.Errorf("%s payload = %s", id, p)
		}
	}

	// The same transcript with the directory excluded: stored name only, nothing queued.
	captureCfg.ExcludeDirs = []string{"/work/demo"}
	e2 := newTailEnv(t, "")
	tailAppend(t, e2.path("sess-c.jsonl"), body)
	tailScan(t, e2.tailer())
	if got := tailCount(t, e2.db, "SELECT count(*) FROM queue"); got != 0 {
		t.Errorf("excluded dir: queue = %d, want 0", got)
	}
	if got := tailCount(t, e2.db, "SELECT count(*) FROM tool_uses WHERE input = '' AND output = '' AND tool_name <> ''"); got != 3 {
		t.Errorf("excluded dir: name-only rows = %d, want 3", got)
	}
}
