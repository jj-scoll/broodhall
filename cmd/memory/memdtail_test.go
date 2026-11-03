package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var tailStart = time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)

func tailTS(h, m, s int) string {
	return time.Date(2026, 10, 28, h, m, s, 0, time.UTC).Format("2006-01-02T15:04:05.000Z")
}

func tailPrompt(sess, text, ts, entry string) string {
	return fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":"/work/demo","entrypoint":%q,"timestamp":%q,"message":{"role":"user","content":%q}}`+"\n", sess, entry, ts, text)
}

func tailToolUse(sess, id, name, ts string) string {
	return fmt.Sprintf(`{"type":"assistant","sessionId":%q,"cwd":"/work/demo","entrypoint":"cli","timestamp":%q,"message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":%q,"name":%q,"input":{"file_path":"/work/demo/a.go"}}]}}`+"\n", sess, ts, id, name)
}

func tailResult(sess, id, ts string) string {
	return fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":"/work/demo","entrypoint":"cli","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"ok"}]}}`+"\n", sess, ts, id)
}

// tailPair is a tool_use line plus its tool_result line.
func tailPair(sess, id, ts string) string {
	return tailToolUse(sess, id, "Read", ts) + tailResult(sess, id, ts)
}

type tailEnv struct {
	db   *sql.DB
	root string
	dir  string // directory holding transcripts
}

func newTailEnv(t *testing.T, sub string) *tailEnv {
	t.Helper()
	db, _ := openTestMemd(t)
	root := t.TempDir()
	dir := filepath.Join(root, "projects", "-work-demo")
	if sub != "" {
		dir = filepath.Join(dir, sub)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return &tailEnv{db: db, root: root, dir: dir}
}

func (e *tailEnv) tailer() *memdTailer {
	return newMemdTailer(e.db, map[string]string{e.root: "acct"}, tailStart)
}

func (e *tailEnv) path(name string) string { return filepath.Join(e.dir, name) }

func tailAppend(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func tailScan(t *testing.T, tl *memdTailer) int {
	t.Helper()
	n, err := tl.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return n
}

func tailCount(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestMemdTailAppendInTwoWrites(t *testing.T) {
	e := newTailEnv(t, "")
	p := e.path("sess-a.jsonl")
	tl := e.tailer()

	tailAppend(t, p, tailPrompt("sess-a", "first", tailTS(10, 0, 0), "cli")+tailPair("sess-a", "t1", tailTS(10, 0, 1)))
	if n := tailScan(t, tl); n != 2 {
		t.Errorf("first Scan newEvents = %d, want 2 (prompt + tool_use)", n)
	}
	tailAppend(t, p, tailPrompt("sess-a", "second", tailTS(10, 1, 0), "cli")+tailPair("sess-a", "t2", tailTS(10, 1, 1)))
	if n := tailScan(t, tl); n != 2 {
		t.Errorf("second Scan newEvents = %d, want 2", n)
	}
	if n := tailScan(t, tl); n != 0 {
		t.Errorf("idle Scan newEvents = %d, want 0", n)
	}
	for q, want := range map[string]int{
		"SELECT count(*) FROM prompts":                                                2,
		"SELECT count(*) FROM tool_uses":                                              2,
		"SELECT count(*) FROM queue WHERE state='queued'":                             2,
		"SELECT count(*) FROM sessions WHERE session_id='sess-a' AND NOT is_observer": 1,
	} {
		if got := tailCount(t, e.db, q); got != want {
			t.Errorf("%s = %d, want %d", q, got, want)
		}
	}
	// prompt_number counts prompts per session.
	var nums []int
	rows, err := e.db.Query("SELECT prompt_number FROM prompts WHERE session_id='sess-a' ORDER BY created_at")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n int
		rows.Scan(&n)
		nums = append(nums, n)
	}
	if fmt.Sprint(nums) != "[1 2]" {
		t.Errorf("prompt_numbers = %v, want [1 2]", nums)
	}
}

func TestMemdTailPartialLastLine(t *testing.T) {
	e := newTailEnv(t, "")
	p := e.path("sess-p.jsonl")
	tl := e.tailer()

	first := tailPrompt("sess-p", "one", tailTS(10, 0, 0), "cli")
	second := tailPrompt("sess-p", "two", tailTS(10, 0, 5), "cli")
	cut := len(second) / 2
	tailAppend(t, p, first+second[:cut])
	if n := tailScan(t, tl); n != 1 {
		t.Fatalf("Scan with partial tail newEvents = %d, want 1", n)
	}
	var off int64
	if err := e.db.QueryRow("SELECT byte_offset FROM transcript_offsets WHERE path=?", p).Scan(&off); err != nil {
		t.Fatal(err)
	}
	if off != int64(len(first)) {
		t.Errorf("offset = %d, want %d (end of last complete line)", off, len(first))
	}
	tailAppend(t, p, second[cut:])
	if n := tailScan(t, tl); n != 1 {
		t.Errorf("Scan after completing line newEvents = %d, want 1", n)
	}
	if got := tailCount(t, e.db, "SELECT count(*) FROM prompts"); got != 2 {
		t.Errorf("prompts = %d, want 2", got)
	}
}

func TestMemdTailRereadNoDuplicates(t *testing.T) {
	body := tailPrompt("sess-r", "one", tailTS(10, 0, 0), "cli") + tailPair("sess-r", "t1", tailTS(10, 0, 1))
	orig := body + tailPrompt("sess-r", "two", tailTS(10, 5, 0), "cli")
	tests := []struct {
		name   string
		mutate func(t *testing.T, p string)
	}{
		{"truncate and rewrite in place", func(t *testing.T, p string) {
			// Same inode, now smaller than the stored size: re-read from 0.
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"replace file with a new inode", func(t *testing.T, p string) {
			tmp := p + ".new"
			if err := os.WriteFile(tmp, []byte(orig), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(tmp, p); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTailEnv(t, "")
			p := e.path("sess-r.jsonl")
			tl := e.tailer()
			tailAppend(t, p, orig)
			tailScan(t, tl)
			tt.mutate(t, p)
			tailScan(t, tl)
			for q, want := range map[string]int{
				"SELECT count(*) FROM tool_uses":         1,
				"SELECT count(*) FROM queue":             1,
				"SELECT count(*) FROM prompts":           2,
				"SELECT max(prompt_number) FROM prompts": 2,
			} {
				if got := tailCount(t, e.db, q); got != want {
					t.Errorf("%s = %d, want %d", q, got, want)
				}
			}
		})
	}
}

func TestMemdTailObserverPathNoQueue(t *testing.T) {
	e := newTailEnv(t, "memory-observer-cwd")
	tailAppend(t, e.path("sess-o.jsonl"), tailPrompt("sess-o", "obs", tailTS(10, 0, 0), "cli")+tailPair("sess-o", "t1", tailTS(10, 0, 1)))
	tailScan(t, e.tailer())
	if got := tailCount(t, e.db, "SELECT count(*) FROM queue"); got != 0 {
		t.Errorf("queue = %d, want 0 for an observer transcript", got)
	}
	if got := tailCount(t, e.db, "SELECT count(*) FROM tool_uses"); got != 1 {
		t.Errorf("tool_uses = %d, want 1 (still stored)", got)
	}
	if got := tailCount(t, e.db, "SELECT count(*) FROM sessions WHERE session_id='sess-o' AND is_observer"); got != 1 {
		t.Errorf("observer session rows = %d, want 1", got)
	}
}

func TestMemdTailSubagentNoQueue(t *testing.T) {
	e := newTailEnv(t, "subagents")
	tailAppend(t, e.path("agent-1.jsonl"), tailPair("sess-s", "t1", tailTS(10, 0, 1)))
	tailScan(t, e.tailer())
	if got := tailCount(t, e.db, "SELECT count(*) FROM queue"); got != 0 {
		t.Errorf("queue = %d, want 0 for a subagent transcript", got)
	}
	if got := tailCount(t, e.db, "SELECT count(*) FROM tool_uses"); got != 1 {
		t.Errorf("tool_uses = %d, want 1", got)
	}
}

func TestMemdTailSkipsBeforeStartedAt(t *testing.T) {
	e := newTailEnv(t, "")
	old := time.Date(2026, 10, 27, 23, 59, 0, 0, time.UTC).Format("2006-01-02T15:04:05.000Z")
	tailAppend(t, e.path("sess-e.jsonl"), tailPrompt("sess-e", "old", old, "cli")+tailPair("sess-e", "told", old)+
		tailPrompt("sess-e", "new", tailTS(10, 0, 0), "cli")+tailPair("sess-e", "tnew", tailTS(10, 0, 1)))
	if n := tailScan(t, e.tailer()); n != 2 {
		t.Errorf("newEvents = %d, want 2 (only events at or after startedAt)", n)
	}
	if got := tailCount(t, e.db, "SELECT count(*) FROM tool_uses WHERE tool_use_id='told'"); got != 0 {
		t.Errorf("old tool_use stored")
	}
	if got := tailCount(t, e.db, "SELECT count(*) FROM prompts WHERE text='old'"); got != 0 {
		t.Errorf("old prompt stored")
	}
}

// Sessions (and the observations that copy their project) are keyed like save_memory and
// session_start_context: projectKey of the transcript cwd, not the raw path.
func TestMemdTailSessionProjectIsKey(t *testing.T) {
	e := newTailEnv(t, "")
	tailAppend(t, e.path("sess-k.jsonl"), tailPair("sess-k", "t1", tailTS(10, 0, 1)))
	tailScan(t, e.tailer())
	var project, cwd string
	if err := e.db.QueryRow("SELECT project, cwd FROM sessions WHERE session_id='sess-k'").Scan(&project, &cwd); err != nil {
		t.Fatal(err)
	}
	if project != projectKey("/work/demo") || cwd != "/work/demo" {
		t.Errorf("project, cwd = %q, %q; want %q, /work/demo", project, cwd, projectKey("/work/demo"))
	}
}

// A transcript last written before startedAt holds nothing to record, so the first pass must
// skip to its end without reading it: reading thousands of old files delayed live capture by
// over ten minutes. The lines are stamped after startedAt only to prove they were not read.
// Later appends to the same file are still tailed.
func TestMemdTailSkipsFilesUntouchedSinceStart(t *testing.T) {
	e := newTailEnv(t, "")
	p := e.path("sess-z.jsonl")
	tl := e.tailer()
	tailAppend(t, p, tailPrompt("sess-z", "unread", tailTS(10, 0, 0), "cli")+tailPair("sess-z", "tunread", tailTS(10, 0, 1)))
	old := tailStart.Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if n := tailScan(t, tl); n != 0 {
		t.Errorf("first Scan newEvents = %d, want 0 (file untouched since startedAt)", n)
	}
	tailAppend(t, p, tailPair("sess-z", "tlive", tailTS(10, 5, 0)))
	if n := tailScan(t, tl); n != 1 {
		t.Errorf("Scan after append newEvents = %d, want 1", n)
	}
	if got := tailCount(t, e.db, "SELECT count(*) FROM tool_uses WHERE tool_use_id='tlive'"); got != 1 {
		t.Errorf("appended tool_use not stored")
	}
	if got := tailCount(t, e.db, "SELECT count(*) FROM tool_uses WHERE tool_use_id='tunread'"); got != 0 {
		t.Errorf("tool_use from before the skip was read")
	}
}

// mtime comes from the kernel's coarse clock, which lags time.Now() by a few ms, so a file
// written right after startup can carry an mtime just before startedAt. It must still be read.
func TestMemdTailReadsFilesWrittenAroundStart(t *testing.T) {
	e := newTailEnv(t, "")
	p := e.path("sess-j.jsonl")
	tailAppend(t, p, tailPair("sess-j", "tj", tailTS(10, 0, 1)))
	jiffy := tailStart.Add(-10 * time.Millisecond)
	if err := os.Chtimes(p, jiffy, jiffy); err != nil {
		t.Fatal(err)
	}
	if n := tailScan(t, e.tailer()); n != 1 {
		t.Errorf("newEvents = %d, want 1 (file written around startup)", n)
	}
}

// Lines written while the daemon was down sit between a file's stored offset and its end,
// stamped (and mtime'd) before the new startedAt. A file with a stored offset resumes from it
// and keeps them; only files with no stored offset are skipped by mtime and filtered by
// startedAt.
func TestMemdTailRestartKeepsDowntimeEvents(t *testing.T) {
	e := newTailEnv(t, "")
	p := e.path("sess-g.jsonl")
	tailAppend(t, p, tailPrompt("sess-g", "before", tailTS(10, 0, 0), "cli")+tailPair("sess-g", "tbefore", tailTS(10, 0, 1)))
	tailScan(t, e.tailer())

	// Daemon down: the session goes on writing, and a session the tailer never saw starts.
	down := tailStart.Add(10*time.Hour + 30*time.Minute)
	tailAppend(t, p, tailPrompt("sess-g", "during", tailTS(10, 30, 0), "cli")+tailPair("sess-g", "tduring", tailTS(10, 30, 1)))
	fresh := e.path("sess-f.jsonl")
	tailAppend(t, fresh, tailPair("sess-f", "tfresh", tailTS(10, 30, 2)))
	for _, f := range []string{p, fresh} {
		if err := os.Chtimes(f, down, down); err != nil {
			t.Fatal(err)
		}
	}

	restarted := newMemdTailer(e.db, map[string]string{e.root: "acct"}, tailStart.Add(11*time.Hour))
	if n := tailScan(t, restarted); n != 2 {
		t.Errorf("Scan after restart newEvents = %d, want 2 (the prompt and tool_use written while down)", n)
	}
	for q, want := range map[string]int{
		"SELECT count(*) FROM tool_uses WHERE tool_use_id='tduring'": 1,
		"SELECT count(*) FROM prompts WHERE text='during'":           1,
		"SELECT count(*) FROM tool_uses WHERE tool_use_id='tfresh'":  0,
	} {
		if got := tailCount(t, e.db, q); got != want {
			t.Errorf("%s = %d, want %d", q, got, want)
		}
	}
}

// An observer latch seen in an early part of a file must survive a tailer restart, so later
// batches without any sdk* line still produce no queue rows.
func TestMemdTailObserverLatchSurvivesRestart(t *testing.T) {
	e := newTailEnv(t, "")
	p := e.path("sess-l.jsonl")
	tailAppend(t, p, tailPrompt("sess-l", "observe", tailTS(10, 0, 0), "sdk-cli"))
	tailScan(t, e.tailer())

	tailAppend(t, p, tailPair("sess-l", "t1", tailTS(10, 0, 1)))
	tailScan(t, e.tailer()) // new tailer on the same DB: a restart
	if got := tailCount(t, e.db, "SELECT count(*) FROM tool_uses"); got != 1 {
		t.Fatalf("tool_uses = %d, want 1 (the batch was processed)", got)
	}
	if got := tailCount(t, e.db, "SELECT count(*) FROM queue"); got != 0 {
		t.Errorf("queue = %d, want 0 after restart", got)
	}
	var obs bool
	if err := e.db.QueryRow("SELECT observer FROM transcript_offsets WHERE path=?", p).Scan(&obs); err != nil || !obs {
		t.Errorf("persisted observer = %v (err %v), want true", obs, err)
	}
}

func TestMemdTailPendingEviction(t *testing.T) {
	t.Run("older than an hour behind newest line", func(t *testing.T) {
		e := newTailEnv(t, "")
		p := e.path("sess-v.jsonl")
		tl := e.tailer()
		tailAppend(t, p, tailToolUse("sess-v", "stale", "Grep", tailTS(10, 0, 0))+
			tailToolUse("sess-v", "fresh", "Edit", tailTS(10, 50, 0))+
			tailPrompt("sess-v", "later", tailTS(11, 30, 0), "cli"))
		tailScan(t, tl)
		if got := len(tl.files[p].st.Pending); got != 1 {
			t.Errorf("pending = %d, want 1 (only fresh survives)", got)
		}
		var detail, sess string
		if err := e.db.QueryRow("SELECT detail, session_id FROM loss_receipts WHERE kind='pending_evicted'").Scan(&detail, &sess); err != nil {
			t.Fatalf("receipt: %v", err)
		}
		if detail != "Grep" || sess != "sess-v" {
			t.Errorf("receipt = %q/%q, want Grep/sess-v", detail, sess)
		}
	})
	t.Run("cap of 1000 evicts oldest first", func(t *testing.T) {
		e := newTailEnv(t, "")
		p := e.path("sess-c.jsonl")
		tl := e.tailer()
		var b strings.Builder
		for i := 0; i < 1005; i++ { // 1005 s apart in total, well inside one hour
			ts := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second).Format("2006-01-02T15:04:05.000Z")
			b.WriteString(tailToolUse("sess-c", fmt.Sprintf("t%04d", i), fmt.Sprintf("Tool%04d", i), ts))
		}
		tailAppend(t, p, b.String())
		tailScan(t, tl)
		pend := tl.files[p].st.Pending
		if len(pend) != 1000 {
			t.Fatalf("pending = %d, want 1000", len(pend))
		}
		if _, ok := pend["t0004"]; ok {
			t.Errorf("t0004 should be evicted")
		}
		if _, ok := pend["t0005"]; !ok {
			t.Errorf("t0005 should survive")
		}
		if got := tailCount(t, e.db, "SELECT count(*) FROM loss_receipts WHERE kind='pending_evicted' AND detail IN ('Tool0000','Tool0001','Tool0002','Tool0003','Tool0004')"); got != 5 {
			t.Errorf("receipts for the 5 oldest = %d, want 5", got)
		}
		if got := tailCount(t, e.db, "SELECT count(*) FROM loss_receipts"); got != 5 {
			t.Errorf("receipts = %d, want exactly 5", got)
		}
	})
}

func TestMemdTailRunPicksUpAppends(t *testing.T) {
	e := newTailEnv(t, "")
	tl := e.tailer()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tl.Run(ctx) }()

	time.Sleep(300 * time.Millisecond) // let the watcher attach
	tailAppend(t, e.path("sess-run.jsonl"), tailPair("sess-run", "t1", tailTS(10, 0, 1)))
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if tailCount(t, e.db, "SELECT count(*) FROM queue") == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := tailCount(t, e.db, "SELECT count(*) FROM queue"); got != 1 {
		t.Errorf("queue = %d, want 1", got)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil on cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// A subagent transcript carries the parent's session id, but its prompts are briefs written
// by the parent's model, not typed by the user: they stay out of prompts and do not shift the
// parent's prompt_number. UserPromptSubmit does not fire for subagents either.
func TestMemdTailSubagentPromptsNotStored(t *testing.T) {
	e := newTailEnv(t, "")
	sub := filepath.Join(e.dir, "sess-p", "subagents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	tailAppend(t, e.path("sess-p.jsonl"), tailPrompt("sess-p", "typed one", tailTS(10, 0, 0), "cli"))
	tailAppend(t, filepath.Join(sub, "agent-1.jsonl"), tailPrompt("sess-p", "You are writing paraphrases", tailTS(10, 0, 5), "cli"))
	// Sidechain latch on a file outside subagents/.
	tailAppend(t, e.path("sess-q.jsonl"), strings.Replace(tailPrompt("sess-q", "side brief", tailTS(10, 0, 6), "cli"), `{"type"`, `{"isSidechain":true,"type"`, 1))
	tailAppend(t, e.path("sess-p.jsonl"), tailPrompt("sess-p", "typed two", tailTS(10, 1, 0), "cli"))
	tailScan(t, e.tailer())
	if got := tailCount(t, e.db, "SELECT count(*) FROM prompts WHERE text IN ('You are writing paraphrases','side brief')"); got != 0 {
		t.Errorf("subagent prompts stored = %d, want 0", got)
	}
	if got := tailCount(t, e.db, "SELECT count(*) FROM prompts WHERE session_id='sess-p' AND text='typed two' AND prompt_number=2"); got != 1 {
		t.Errorf("parent prompt_number sequence disturbed")
	}
}
