package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// impReply is an assistant text line that ends the turn (a live tailer would arm a summary).
func impReply(sess, uuid, text, ts string) string {
	return fmt.Sprintf(`{"type":"assistant","uuid":%q,"sessionId":%q,"cwd":"/work/demo","gitBranch":"feat/x","entrypoint":"cli","timestamp":%q,"message":{"role":"assistant","model":"claude-opus-5","stop_reason":"end_turn","content":[{"type":"text","text":%q}]}}`+"\n", uuid, sess, ts, text)
}

// impHistory is one finished session: open marker, prompt, one tool call, a closing reply.
func impHistory(sess string) string {
	return forkOpen(sess, tailTS(9, 0, 0)) +
		forkPrompt(sess, "u-"+sess, "fix the parser "+sess, tailTS(9, 0, 1), "") +
		tailPair(sess, "tool-"+sess, tailTS(9, 0, 2)) +
		impReply(sess, "r-"+sess, "done, the parser is fixed", tailTS(9, 0, 3))
}

// impOld writes a transcript and ages it, so a tailer started now skips it.
func impOld(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

// forbidClaude makes any observer or summary call fail the test.
func forbidClaude(t *testing.T) *int {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	old := runClaude
	t.Cleanup(func() { runClaude = old })
	runClaude = func(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		t.Errorf("runClaude called during import: the observer must never run for imported history")
		return nil, fmt.Errorf("forbidden")
	}
	return &calls
}

func (e *tailEnv) importer(tl *memdTailer) *memdImporter {
	var fr func(string) (int64, bool)
	if tl != nil {
		fr = tl.frontier
	}
	return newMemdImporter(e.db, map[string]string{e.root: "acct"}, fr)
}

func runImport(t *testing.T, imp *memdImporter, dry bool) importStatus {
	t.Helper()
	if err := imp.Start(context.Background(), dry); err != nil {
		t.Fatalf("Start: %v", err)
	}
	imp.Wait()
	st := imp.Status()
	if st.Running {
		t.Fatalf("still running after Wait: %+v", st)
	}
	if st.Err != "" {
		t.Fatalf("import error: %s", st.Err)
	}
	return st
}

func TestImportWritesHistoryAndNeverQueues(t *testing.T) {
	calls := forbidClaude(t)
	e := newTailEnv(t, "")
	impOld(t, e.path("sess-h.jsonl"), impHistory("sess-h"))
	tl := e.tailer()
	tailScan(t, tl) // the old file is skipped: the tailer's frontier is its end

	// The real workers run while the import does, so a queue or summary row would be taken.
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); runObserverWorkers(ctx, e.db, 0) }()
	go func() { defer wg.Done(); runSummaryWorker(ctx, e.db) }()

	st := runImport(t, e.importer(tl), false)
	time.Sleep(300 * time.Millisecond)
	cancel()
	wg.Wait()

	if st.FilesDone != 1 || st.BytesDone == 0 || st.BytesDone != st.BytesTotal {
		t.Errorf("status = %+v, want 1 file done and all bytes read", st)
	}
	for q, want := range map[string]int{
		"SELECT count(*) FROM prompts WHERE session_id='sess-h'":                                                     1,
		"SELECT count(*) FROM tool_uses WHERE session_id='sess-h'":                                                   1,
		"SELECT count(*) FROM replies WHERE session_id='sess-h'":                                                     1,
		"SELECT count(*) FROM sessions WHERE session_id='sess-h' AND git_branch='feat/x' AND model='claude-opus-5'":  1,
		"SELECT count(*) FROM sessions WHERE session_id='sess-h' AND cwd='/work/demo' AND project <> ''":             1,
		"SELECT count(*) FROM sessions WHERE session_id='sess-h' AND root_uuid='u-sess-h' AND opened_at IS NOT NULL": 1,
		"SELECT count(*) FROM queue":            0,
		"SELECT count(*) FROM summary_requests": 0,
		"SELECT count(*) FROM observations":     0,
		"SELECT count(*) FROM summaries":        0,
	} {
		if got := tailCount(t, e.db, q); got != want {
			t.Errorf("%s = %d, want %d", q, got, want)
		}
	}
	if *calls != 0 {
		t.Errorf("runClaude was called %d times", *calls)
	}
	// The session can be resumed: it has a transcript_offsets row at the end of the file.
	fi, _ := os.Stat(e.path("sess-h.jsonl"))
	var off int64
	if err := e.db.QueryRow(`SELECT byte_offset FROM transcript_offsets WHERE path = ?`, e.path("sess-h.jsonl")).Scan(&off); err != nil || off != fi.Size() {
		t.Errorf("transcript_offsets offset = %d (err %v), want %d", off, err, fi.Size())
	}
}

func TestImportIsIdempotentAndIncremental(t *testing.T) {
	forbidClaude(t)
	e := newTailEnv(t, "")
	impOld(t, e.path("sess-a.jsonl"), impHistory("sess-a"))
	imp := e.importer(nil) // no tailer: no frontier
	runImport(t, imp, false)
	counts := func() string {
		var s []string
		for _, q := range []string{"prompts", "tool_uses", "replies", "sessions", "queue", "summary_requests"} {
			s = append(s, fmt.Sprint(tailCount(t, e.db, "SELECT count(*) FROM "+q)))
		}
		return strings.Join(s, ",")
	}
	first := counts()

	st := runImport(t, imp, false)
	if st.BytesDone != 0 || st.Rows != 0 {
		t.Errorf("second run read %d bytes and added %d rows, want 0 and 0", st.BytesDone, st.Rows)
	}
	if got := counts(); got != first {
		t.Errorf("counts after a re-run = %s, want %s", got, first)
	}

	// New material is picked up: an appended line and a new file.
	tailAppend(t, e.path("sess-a.jsonl"), forkPrompt("sess-a", "u-late", "one more thing", tailTS(9, 5, 0), ""))
	impOld(t, e.path("sess-b.jsonl"), impHistory("sess-b"))
	st = runImport(t, imp, false)
	if st.FilesDone != 2 || st.Rows == 0 {
		t.Errorf("third run status = %+v, want 2 files and new rows", st)
	}
	if got := tailCount(t, e.db, "SELECT count(*) FROM prompts"); got != 3 {
		t.Errorf("prompts = %d, want 3", got)
	}
	if got := tailCount(t, e.db, "SELECT count(*) FROM queue"); got != 0 {
		t.Errorf("queue = %d, want 0", got)
	}
}

func TestLiveTailingAfterImportQueuesOnlyLiveWork(t *testing.T) {
	forbidClaude(t)
	e := newTailEnv(t, "")
	p := e.path("sess-l.jsonl")
	impOld(t, p, impHistory("sess-l"))
	tl := e.tailer()
	tailScan(t, tl)
	runImport(t, e.importer(tl), false)

	// A restarted tailer resumes at the stored offset: the imported history is behind it.
	tl2 := e.tailer()
	if n := tailScan(t, tl2); n != 0 {
		t.Errorf("restarted Scan inserted %d rows from imported history, want 0", n)
	}
	if got := tailCount(t, e.db, "SELECT count(*) FROM queue"); got != 0 {
		t.Fatalf("queue after restart = %d, want 0", got)
	}
	tailAppend(t, p, tailPair("sess-l", "tool-live", tailTS(11, 0, 0)))
	if n := tailScan(t, tl2); n != 1 {
		t.Errorf("live Scan inserted %d rows, want 1", n)
	}
	if got := tailCount(t, e.db, "SELECT count(*) FROM queue WHERE tool_use_id='tool-live'"); got != 1 {
		t.Errorf("live tool call queue rows = %d, want 1", got)
	}
	if got := tailCount(t, e.db, "SELECT count(*) FROM queue"); got != 1 {
		t.Errorf("queue = %d, want only the live tool call", got)
	}
}

func TestImportLeavesLiveTailerProgressAlone(t *testing.T) {
	forbidClaude(t)
	e := newTailEnv(t, "")
	p := e.path("sess-live.jsonl")
	tailAppend(t, p, tailPrompt("sess-live", "first", tailTS(10, 0, 0), "cli")+tailPair("sess-live", "t1", tailTS(10, 0, 1)))
	tl := e.tailer()
	tailScan(t, tl) // fresh file: read and queued
	var off int64
	e.db.QueryRow(`SELECT byte_offset FROM transcript_offsets WHERE path = ?`, p).Scan(&off)
	queued := tailCount(t, e.db, "SELECT count(*) FROM queue")
	if off == 0 || queued != 1 {
		t.Fatalf("setup: offset %d, queued %d", off, queued)
	}

	// The tailer has not seen the next lines yet; the import must leave them to it.
	tailAppend(t, p, tailPair("sess-live", "t2", tailTS(10, 0, 5)))
	runImport(t, e.importer(tl), false)
	if got := tailCount(t, e.db, "SELECT count(*) FROM tool_uses"); got != 1 {
		t.Errorf("tool_uses after import = %d, want 1 (t2 belongs to the tailer)", got)
	}
	var off2 int64
	e.db.QueryRow(`SELECT byte_offset FROM transcript_offsets WHERE path = ?`, p).Scan(&off2)
	if off2 != off {
		t.Errorf("import moved the tailer's offset from %d to %d", off, off2)
	}
	if n := tailScan(t, tl); n != 1 {
		t.Errorf("tailer inserted %d rows, want 1 (t2, once)", n)
	}
	if got := tailCount(t, e.db, "SELECT count(*) FROM queue WHERE tool_use_id='t2'"); got != 1 {
		t.Errorf("t2 queue rows = %d, want 1: the live call must still reach the observer", got)
	}
	if got := tailCount(t, e.db, "SELECT count(*) FROM tool_uses"); got != 2 {
		t.Errorf("tool_uses = %d, want 2", got)
	}
}

// The live tailer drops events from before it started, so a session it joined late has its
// later prompts numbered first. Import adds the earlier ones; the numbers must follow time.
func TestImportRenumbersPromptsOfLateJoinedSession(t *testing.T) {
	forbidClaude(t)
	e := newTailEnv(t, "")
	p := e.path("sess-n.jsonl")
	tailAppend(t, p, forkPrompt("sess-n", "u-1", "before the daemon", "2026-10-27T10:00:00.000Z", "")+
		forkPrompt("sess-n", "u-2", "after the daemon", tailTS(10, 0, 0), ""))
	tl := e.tailer()
	tailScan(t, tl)
	if got := tailCount(t, e.db, "SELECT count(*) FROM prompts"); got != 1 {
		t.Fatalf("setup: live tailer kept %d prompts, want 1", got)
	}
	runImport(t, e.importer(tl), false)
	rows, err := e.db.Query(`SELECT text FROM prompts WHERE session_id = 'sess-n' ORDER BY prompt_number`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		got = append(got, s)
	}
	if fmt.Sprint(got) != "[before the daemon after the daemon]" {
		t.Errorf("prompts by number = %v, want them in time order", got)
	}
}

func TestImportDefersFilesTheTailerHasNotSeen(t *testing.T) {
	forbidClaude(t)
	e := newTailEnv(t, "")
	tl := e.tailer()
	tailScan(t, tl)
	impOld(t, e.path("sess-new.jsonl"), impHistory("sess-new"))
	st := runImport(t, e.importer(tl), false)
	if st.FilesDeferred != 1 || st.FilesDone != 0 {
		t.Errorf("status = %+v, want the unseen file deferred", st)
	}
	if got := tailCount(t, e.db, "SELECT count(*) FROM prompts"); got != 0 {
		t.Errorf("prompts = %d, want 0", got)
	}
	tailScan(t, tl)
	st = runImport(t, e.importer(tl), false)
	if st.FilesDone != 1 {
		t.Errorf("after the tailer saw it: status = %+v, want 1 file done", st)
	}
}

// dumpRows renders the rows the import and the live tailer both write, in a stable order.
func dumpRows(t *testing.T, db *sql.DB) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT session_id, prompt_number, text, CAST(created_at AS VARCHAR) FROM prompts ORDER BY session_id, prompt_number`,
		`SELECT session_id, tool_use_id, tool_name, input, output, CAST(created_at AS VARCHAR) FROM tool_uses ORDER BY session_id, tool_use_id`,
		`SELECT session_id, reply_key, text FROM replies ORDER BY session_id, reply_key`,
		`SELECT session_id, project, cwd, is_observer, git_branch, model, forked_from, root_uuid, CAST(opened_at AS VARCHAR) FROM sessions ORDER BY session_id`,
	} {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintln(&b, vals...)
		}
		rows.Close()
		b.WriteString("--\n")
	}
	return b.String()
}

// The import goes through the same parser, masking, capture rules and observer and subagent
// handling as live capture: the same transcripts give the same rows.
func TestImportMatchesLiveCapture(t *testing.T) {
	forbidClaude(t)
	secret := "AKIAIOSFODNN7EXAMPLE"
	files := map[string]string{
		"projects/-work-demo/sess-1.jsonl": forkOpen("sess-1", tailTS(9, 0, 0)) +
			forkPrompt("sess-1", "u-1", "use key "+secret+" now", tailTS(9, 0, 1), "") +
			tailToolUse("sess-1", "bash-1", "Bash", tailTS(9, 0, 2)) + tailResult("sess-1", "bash-1", tailTS(9, 0, 3)) +
			tailPair("sess-1", "read-1", tailTS(9, 0, 4)) +
			impReply("sess-1", "r-1", "key is "+secret, tailTS(9, 0, 5)),
		"projects/-work-demo/sess-1/subagents/agent-x.jsonl": tailPrompt("sess-1", "brief for the agent", tailTS(9, 1, 0), "cli") +
			tailPair("sess-1", "sub-1", tailTS(9, 1, 1)) + impReply("sess-1", "sr-1", "sub says hi", tailTS(9, 1, 2)),
		"projects/-work-memory-observer-cwd/sess-o.jsonl": tailPrompt("sess-o", "observer payload", tailTS(9, 2, 0), "sdk-cli") +
			tailPair("sess-o", "obs-1", tailTS(9, 2, 1)) + impReply("sess-o", "or-1", "observer reply", tailTS(9, 2, 2)),
		"projects/-work-demo/sess-fork.jsonl": forkOpen("sess-fork", tailTS(10, 0, 0)) +
			forkPrompt("sess-fork", "u-1", "use key "+secret+" now", tailTS(9, 0, 1), "") +
			forkPrompt("sess-fork", "u-2", "forked ask", tailTS(10, 0, 5), ""),
	}
	write := func(root string) {
		for rel, body := range files {
			impOld(t, filepath.Join(root, rel), body)
		}
	}

	// Live: a tailer started before the events reads the files from 0.
	live := newTailEnv(t, "")
	write(live.root)
	// impOld aged the files, so open them for the tailer by touching.
	filepath.WalkDir(live.root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			os.Chtimes(p, time.Now(), time.Now())
		}
		return nil
	})
	tailScan(t, live.tailer())
	want := dumpRows(t, live.db)
	if strings.Contains(want, secret) {
		t.Fatalf("live capture kept the secret: the fixture does not test masking")
	}

	imp := newTailEnv(t, "")
	write(imp.root)
	tl := imp.tailer()
	tailScan(t, tl)
	runImport(t, imp.importer(tl), false)
	if got := dumpRows(t, imp.db); got != want {
		t.Errorf("imported rows differ from live capture\n--- live\n%s--- import\n%s", want, got)
	}
	if got := tailCount(t, imp.db, "SELECT count(*) FROM queue"); got != 0 {
		t.Errorf("queue = %d, want 0", got)
	}
	if got := tailCount(t, imp.db, "SELECT count(*) FROM sessions WHERE session_id='sess-o' AND is_observer"); got != 1 {
		t.Errorf("observer session not flagged")
	}
	if got := tailCount(t, imp.db, "SELECT count(*) FROM prompts WHERE text LIKE 'brief for the agent%'"); got != 0 {
		t.Errorf("a subagent brief was imported as a prompt")
	}
}

func TestImportRefusesSecondConcurrentRun(t *testing.T) {
	forbidClaude(t)
	e := newTailEnv(t, "")
	impOld(t, e.path("sess-c.jsonl"), impHistory("sess-c"))
	imp := e.importer(nil)
	hold, release := make(chan struct{}), make(chan struct{})
	importAfterChunk = func() {
		select {
		case hold <- struct{}{}:
		default:
		}
		<-release
	}
	t.Cleanup(func() { importAfterChunk = nil })
	if err := imp.Start(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	<-hold
	if err := imp.Start(context.Background(), false); err != errImportRunning {
		t.Errorf("second Start = %v, want errImportRunning", err)
	}
	if !imp.Status().Running {
		t.Errorf("Status().Running = false while an import runs")
	}
	close(release)
	imp.Wait()
	if st := imp.Status(); st.Running || st.FilesDone != 1 {
		t.Errorf("final status = %+v", st)
	}
}

func TestImportDryRunWritesNothing(t *testing.T) {
	forbidClaude(t)
	e := newTailEnv(t, "")
	impOld(t, e.path("sess-d.jsonl"), impHistory("sess-d"))
	st := runImport(t, e.importer(nil), true)
	if !st.DryRun || st.FilesTotal != 1 || st.BytesTotal == 0 || st.BytesDone != 0 {
		t.Errorf("dry-run status = %+v, want 1 file to read and nothing read", st)
	}
	for _, tbl := range []string{"sessions", "prompts", "tool_uses", "replies", "transcript_offsets", "import_files"} {
		if got := tailCount(t, e.db, "SELECT count(*) FROM "+tbl); got != 0 {
			t.Errorf("%s has %d rows after a dry run", tbl, got)
		}
	}
	st = runImport(t, e.importer(nil), false)
	if st.FilesDone != 1 {
		t.Errorf("real run after dry run: %+v", st)
	}
}

func TestImportBackfillsForkColumnsInOpenedOrder(t *testing.T) {
	forbidClaude(t)
	e := newTailEnv(t, "")
	orig, forkA, forkB := "ccc-orig", "aaa-forkA", "bbb-forkB"
	body := func(sess, open string) string {
		return forkOpen(sess, open) + forkPrompt(sess, "u-1", "shared first message", tailTS(9, 0, 1), "") +
			forkPrompt(sess, "u-"+sess, "own ask "+sess, open, "")
	}
	// File names sort the forks first; the original was opened first.
	impOld(t, e.path("a-fork.jsonl"), body(forkA, tailTS(11, 0, 0)))
	impOld(t, e.path("b-fork.jsonl"), body(forkB, tailTS(12, 0, 0)))
	impOld(t, e.path("z-orig.jsonl"), body(orig, tailTS(9, 0, 0)))
	// A session row written before the fork columns existed.
	if _, err := e.db.Exec(`INSERT INTO sessions (session_id, project, cwd, is_observer, started_at, last_seen_at)
VALUES (?, 'demo', '/work/demo', false, current_timestamp, current_timestamp)`, orig); err != nil {
		t.Fatal(err)
	}
	runImport(t, e.importer(nil), false)
	for sess, want := range map[string]string{orig: "", forkA: orig, forkB: orig} {
		if got := sessionForkedFrom(t, e, sess); got != want {
			t.Errorf("forked_from(%s) = %q, want %q", sess, got, want)
		}
	}
	if got := tailCount(t, e.db, "SELECT count(*) FROM sessions WHERE root_uuid = 'u-1' AND opened_at IS NOT NULL"); got != 3 {
		t.Errorf("sessions with fork facts = %d, want 3", got)
	}
}

// A chunk boundary may fall between a tool_use and its result, and each chunk is its own
// transaction: the result must still be matched and nothing written twice.
func TestImportChunksKeepToolPairsTogether(t *testing.T) {
	forbidClaude(t)
	old := importChunkLines
	importChunkLines = 1
	t.Cleanup(func() { importChunkLines = old })
	chunks := 0
	importAfterChunk = func() { chunks++ }
	t.Cleanup(func() { importAfterChunk = nil })

	e := newTailEnv(t, "")
	impOld(t, e.path("sess-k.jsonl"), impHistory("sess-k"))
	runImport(t, e.importer(nil), false)
	if chunks < 4 {
		t.Errorf("chunks = %d, want one per line", chunks)
	}
	for q, want := range map[string]int{
		"SELECT count(*) FROM prompts": 1, "SELECT count(*) FROM tool_uses": 1, "SELECT count(*) FROM replies": 1, "SELECT count(*) FROM queue": 0,
	} {
		if got := tailCount(t, e.db, q); got != want {
			t.Errorf("%s = %d, want %d", q, got, want)
		}
	}
}

// A tool_use still waiting for its result when the import reaches the end of a file is read
// again on the next run, when the result may have arrived.
func TestImportRereadsFileWithOpenToolCall(t *testing.T) {
	forbidClaude(t)
	e := newTailEnv(t, "")
	p := e.path("sess-o.jsonl")
	impOld(t, p, forkPrompt("sess-o", "u-1", "go", tailTS(9, 0, 0), "")+tailToolUse("sess-o", "t-open", "Read", tailTS(9, 0, 1)))
	imp := e.importer(nil)
	runImport(t, imp, false)
	if got := tailCount(t, e.db, "SELECT count(*) FROM tool_uses"); got != 0 {
		t.Fatalf("tool_uses = %d before its result exists", got)
	}
	tailAppend(t, p, tailResult("sess-o", "t-open", tailTS(9, 0, 2)))
	runImport(t, imp, false)
	if got := tailCount(t, e.db, "SELECT count(*) FROM tool_uses"); got != 1 {
		t.Errorf("tool_uses = %d, want the call matched on the second run", got)
	}
	if got := tailCount(t, e.db, "SELECT count(*) FROM queue"); got != 0 {
		t.Errorf("queue = %d, want 0", got)
	}
}

func TestMigrateLeavesRawIndexToTheBackground(t *testing.T) {
	db, _ := openTestMemd(t)
	if _, err := db.Exec(`INSERT INTO prompts (session_id, prompt_number, text) VALUES ('s', 1, 'findable zebra')`); err != nil {
		t.Fatal(err)
	}
	if err := memdMigrate(db); err != nil {
		t.Fatal(err)
	}
	behind, err := rawBehind(db)
	if err != nil || !behind {
		t.Fatalf("rawBehind after migrate = %v, %v: migrate must not index the backlog", behind, err)
	}
	if err := memdRawIndexBackground(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if behind, _ := rawBehind(db); behind {
		t.Errorf("still behind after the background index")
	}
}

func TestRawCatchUpReleasesTheWriteLockBetweenBatches(t *testing.T) {
	db, _ := openTestMemd(t)
	if _, err := db.Exec(`INSERT INTO prompts (session_id, prompt_number, text, created_at) SELECT 's', i, 'word' || i, TIMESTAMP '2026-01-01' + to_seconds(i) FROM range(1, 1300) t(i)`); err != nil {
		t.Fatal(err)
	}
	steps, free := 0, 0
	rawStepHook = func() {
		steps++
		if memdWriteMu.TryLock() {
			free++
			memdWriteMu.Unlock()
		}
	}
	t.Cleanup(func() { rawStepHook = nil })
	if err := rawCatchUp(db); err != nil {
		t.Fatal(err)
	}
	if steps < 3 || free != steps {
		t.Errorf("steps = %d, lock free after %d of them; want >= 3 batches, lock free after each", steps, free)
	}
	if behind, _ := rawBehind(db); behind {
		t.Errorf("still behind after rawCatchUp")
	}
}

func TestRawBackgroundStopsOnCancel(t *testing.T) {
	db, _ := openTestMemd(t)
	if _, err := db.Exec(`INSERT INTO prompts (session_id, prompt_number, text, created_at) SELECT 's', i, 'word' || i, TIMESTAMP '2026-01-01' + to_seconds(i) FROM range(1, 1300) t(i)`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rawStepHook = func() { cancel() }
	t.Cleanup(func() { rawStepHook = nil })
	if err := memdRawIndexBackground(ctx, db); err == nil {
		t.Errorf("want the context error")
	}
	if behind, _ := rawBehind(db); !behind {
		t.Errorf("index finished despite the cancel")
	}
}

func TestImportEndpointAndCommand(t *testing.T) {
	forbidClaude(t)
	e := newTailEnv(t, "")
	impOld(t, e.path("sess-e.jsonl"), impHistory("sess-e"))
	imp := e.importer(nil)
	memdActiveImporter.Store(imp)
	t.Cleanup(func() { memdActiveImporter.Store(nil) })
	sock := resumeDaemon(t, e.db)

	var out, errw bytes.Buffer
	if code := memImportTo([]string{"--dry-run"}, sock, &out, &errw); code != 0 {
		t.Fatalf("dry-run exit %d: %s", code, errw.String())
	}
	if !strings.Contains(out.String(), "dry run") || !strings.Contains(out.String(), "1 file") {
		t.Errorf("dry-run output = %q", out.String())
	}
	if got := tailCount(t, e.db, "SELECT count(*) FROM prompts"); got != 0 {
		t.Errorf("dry run wrote %d prompts", got)
	}

	out.Reset()
	errw.Reset()
	if code := memImportTo(nil, sock, &out, &errw); code != 0 {
		t.Fatalf("import exit %d: %s", code, errw.String())
	}
	imp.Wait()
	if !strings.Contains(out.String(), "done") || !strings.Contains(out.String(), "1/1 files") {
		t.Errorf("import output = %q", out.String())
	}
	if got := tailCount(t, e.db, "SELECT count(*) FROM prompts"); got != 1 {
		t.Errorf("prompts = %d, want 1", got)
	}

	// A run in progress is refused with a non-zero exit.
	hold, release := make(chan struct{}), make(chan struct{})
	importAfterChunk = func() {
		select {
		case hold <- struct{}{}:
		default:
		}
		<-release
	}
	t.Cleanup(func() { importAfterChunk = nil })
	impOld(t, e.path("sess-f.jsonl"), impHistory("sess-f"))
	if err := imp.Start(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	<-hold
	out.Reset()
	errw.Reset()
	if code := memImportTo(nil, sock, &out, &errw); code == 0 || !strings.Contains(errw.String(), "already running") {
		t.Errorf("concurrent import: exit %d, stderr %q", code, errw.String())
	}
	close(release)
	imp.Wait()
}
