package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var qNow = time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)

// qAdd inserts a tool_uses row and a queued queue row, returning the queue id.
func qAdd(t *testing.T, db *sql.DB, sess, tuid, tool, input string) int64 {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO tool_uses (session_id, tool_use_id, tool_name, input, output) VALUES (?,?,?,?,'out')`,
		sess, tuid, tool, input); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := db.QueryRow(`INSERT INTO queue (session_id, tool_use_id) VALUES (?,?) RETURNING id`, sess, tuid).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func qState(t *testing.T, db *sql.DB, id int64) (state, reason string, attempts int, leased bool) {
	t.Helper()
	var lease sql.NullString
	if err := db.QueryRow(`SELECT state, reason, attempts, CAST(lease_until AS VARCHAR) FROM queue WHERE id=?`, id).
		Scan(&state, &reason, &attempts, &lease); err != nil {
		t.Fatal(err)
	}
	return state, reason, attempts, lease.Valid
}

func TestDedupeKey(t *testing.T) {
	base := dedupeKey("v1", "haiku", "input")
	if base != dedupeKey("v1", "haiku", "input") {
		t.Error("equal inputs must give equal keys")
	}
	if len(base) != 64 {
		t.Errorf("want 64 hex chars, got %d", len(base))
	}
	for name, k := range map[string]string{
		"prompt version": dedupeKey("v2", "haiku", "input"),
		"model":          dedupeKey("v1", "sonnet", "input"),
		"input":          dedupeKey("v1", "haiku", "input2"),
		"separator":      dedupeKey("v1", "haik", "uinput"),
	} {
		if k == base {
			t.Errorf("changing %s must change the key", name)
		}
	}
}

func TestMemdQueueShouldSkip(t *testing.T) {
	orig := skipBashPatterns
	t.Cleanup(func() { skipBashPatterns = orig })
	skipBashPatterns = []*regexp.Regexp{regexp.MustCompile(`^ls\b`)}
	tests := []struct {
		tool, cmd string
		want      bool
	}{
		{"TodoWrite", "", true},
		{"todowrite", "", true}, // matching is case-insensitive
		{"ListMcpResourcesTool", "", true},
		{"SlashCommand", "", true},
		{"Skill", "", true},
		{"AskUserQuestion", "", true},
		{"Edit", "", false},
		{"Bash", "go test ./...", false},
		{"Bash", "ls -la", true},
		{"Read", "ls -la", false}, // patterns apply to Bash only
	}
	for _, tt := range tests {
		if got := shouldSkip(tt.tool, tt.cmd); got != tt.want {
			t.Errorf("shouldSkip(%q,%q)=%v want %v", tt.tool, tt.cmd, got, tt.want)
		}
	}
	skipBashPatterns = orig
	if shouldSkip("Bash", "ls -la") {
		t.Error("the default bash pattern list is empty; nothing should match")
	}
}

// The skip lists come from the settings file: skip_tools is a comma list that replaces the
// default, skip_bash_patterns is one regex, and an invalid regex is ignored.
func TestMemdLoadSkipSettings(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "settings.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	check := func(name string, tools map[string]bool, bash []*regexp.Regexp, tool, cmd string, want bool) {
		t.Helper()
		orig, origB := skipTools, skipBashPatterns
		skipTools, skipBashPatterns = tools, bash
		got := shouldSkip(tool, cmd)
		skipTools, skipBashPatterns = orig, origB
		if got != want {
			t.Errorf("%s: shouldSkip(%q,%q)=%v want %v", name, tool, cmd, got, want)
		}
	}

	tools, bash := loadSkipSettings(filepath.Join(dir, "missing.json"))
	check("missing file", tools, bash, "TodoWrite", "", true)
	check("missing file", tools, bash, "Monitor", "", false)
	check("missing file", tools, bash, "Bash", "ls", false)

	tools, bash = loadSkipSettings(write(`{"skip_tools": "TodoWrite, monitor ,Glob,,mcp__claude-in-chrome__computer",
		"skip_bash_patterns": " ^(rtk )?(ls|pwd|cat|date|git (status|diff|log)|rg|fd)\\b ", "mode": "code"}`))
	check("settings", tools, bash, "Monitor", "", true)
	check("settings", tools, bash, "glob", "", true)
	check("settings", tools, bash, "mcp__claude-in-chrome__computer", "", true)
	check("settings", tools, bash, "Skill", "", false) // the setting replaces the default list
	check("settings", tools, bash, "Bash", "rtk git status", true)
	check("settings", tools, bash, "Bash", "rg -n foo", true)
	check("settings", tools, bash, "Bash", "go test ./...", false)
	check("settings", tools, bash, "Bash", "rgx", false)

	tools, bash = loadSkipSettings(write(`{"skip_tools": "Monitor", "skip_bash_patterns": "(?<=x)ls"}`))
	check("bad regex", tools, bash, "Monitor", "", true)
	check("bad regex", tools, bash, "Bash", "xls", false)

	tools, bash = loadSkipSettings(write(`{"skip_tools": ""}`))
	check("empty list", tools, bash, "TodoWrite", "", false)

	tools, bash = loadSkipSettings(write(`{"skip_tools": 7}`))
	check("non-string", tools, bash, "TodoWrite", "", true)

	tools, bash = loadSkipSettings(write(`not json`))
	check("malformed", tools, bash, "TodoWrite", "", true)
}

func TestMemdQueueClaimSkipsAndRecordsLoss(t *testing.T) {
	db, _ := openTestMemd(t)
	orig := skipBashPatterns
	t.Cleanup(func() { skipBashPatterns = orig })
	skipBashPatterns = []*regexp.Regexp{regexp.MustCompile(`^ls\b`)}

	skipTool := qAdd(t, db, "s", "t1", "TodoWrite", `{}`)
	skipBash := qAdd(t, db, "s", "t2", "Bash", `{"command":"  ls   -la "}`)
	keep := qAdd(t, db, "s", "t3", "Edit", `{"file_path":"/a"}`)

	it, err := claimNext(db, qNow, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if it == nil || it.ID != keep {
		t.Fatalf("want row %d, got %+v", keep, it)
	}
	if it.ToolName != "Edit" || it.Input != `{"file_path":"/a"}` || it.Output != "out" || it.SessionID != "s" || it.ToolUseID != "t3" || it.Attempts != 1 {
		t.Errorf("item fields wrong: %+v", it)
	}
	for _, id := range []int64{skipTool, skipBash} {
		st, reason, _, leased := qState(t, db, id)
		if st != "done" || reason != "skip" || leased {
			t.Errorf("row %d: state=%s reason=%s leased=%v", id, st, reason, leased)
		}
	}
	var n, sum int
	if err := db.QueryRow(`SELECT count(*), sum(n) FROM loss_receipts WHERE kind='skip'`).Scan(&n, &sum); err != nil {
		t.Fatal(err)
	}
	if n != 2 || sum != 2 {
		t.Errorf("want 2 skip receipts totalling 2, got rows=%d sum=%d", n, sum)
	}
	if again, err := claimNext(db, qNow, time.Minute); err != nil || again != nil {
		t.Errorf("empty queue: want nil,nil got %v,%v", again, err)
	}
}

func TestMemdQueueClaimLeaseAndOrder(t *testing.T) {
	db, _ := openTestMemd(t)
	a := qAdd(t, db, "s", "t1", "Edit", `{}`)
	b := qAdd(t, db, "s", "t2", "Edit", `{}`)
	it, err := claimNext(db, qNow, 5*time.Minute)
	if err != nil || it == nil || it.ID != a {
		t.Fatalf("want lowest id %d, got %+v err=%v", a, it, err)
	}
	var ok bool
	if err := db.QueryRow(`SELECT lease_until = CAST(? AS TIMESTAMP) AND state='claimed' FROM queue WHERE id=?`,
		memdTS(qNow.Add(5*time.Minute)), a).Scan(&ok); err != nil || !ok {
		t.Errorf("lease_until must be now+lease stored as UTC (ok=%v err=%v)", ok, err)
	}
	it2, _ := claimNext(db, qNow, time.Minute)
	if it2 == nil || it2.ID != b {
		t.Fatalf("second claim want %d got %+v", b, it2)
	}
}

func TestMemdQueueReclaimExpiredOnce(t *testing.T) {
	db, _ := openTestMemd(t)
	id := qAdd(t, db, "s", "t1", "Edit", `{}`)
	if _, err := claimNext(db, qNow, time.Minute); err != nil {
		t.Fatal(err)
	}
	if n, err := reclaimExpired(db, qNow.Add(30*time.Second)); err != nil || n != 0 {
		t.Fatalf("live lease must not be reclaimed: n=%d err=%v", n, err)
	}
	late := qNow.Add(2 * time.Minute)
	if n, err := reclaimExpired(db, late); err != nil || n != 1 {
		t.Fatalf("want 1 reclaimed, got %d err=%v", n, err)
	}
	if n, err := reclaimExpired(db, late); err != nil || n != 0 {
		t.Fatalf("second reclaim at same now must be 0, got %d err=%v", n, err)
	}
	st, _, attempts, leased := qState(t, db, id)
	if st != "queued" || attempts != 1 || leased {
		t.Errorf("state=%s attempts=%d leased=%v", st, attempts, leased)
	}
	it, err := claimNext(db, late, time.Minute)
	if err != nil || it == nil || it.ID != id || it.Attempts != 2 {
		t.Errorf("reclaimed row must be claimable with attempts=2, got %+v err=%v", it, err)
	}
}

func TestMemdQueueDedupeHitFinishesWithoutReturning(t *testing.T) {
	db, _ := openTestMemd(t)
	dup := qAdd(t, db, "s", "t1", "Edit", `{}`)
	next := qAdd(t, db, "s", "t2", "Edit", `{}`)
	key := dedupeKey("v1", "haiku", "x")
	if _, err := db.Exec(`UPDATE queue SET dedupe_key=? WHERE id=?`, key, dup); err != nil {
		t.Fatal(err)
	}
	// First claim takes the row (no observation yet), then its lease expires...
	if it, err := claimNext(db, qNow, time.Minute); err != nil || it == nil || it.ID != dup || it.DedupeKey != key {
		t.Fatalf("first claim: %+v err=%v", it, err)
	}
	// ...and meanwhile another worker stored the observation with that key.
	if _, err := db.Exec(`INSERT INTO observations (dedupe_key, source) VALUES (?, 'live')`, key); err != nil {
		t.Fatal(err)
	}
	late := qNow.Add(time.Hour)
	if n, err := reclaimExpired(db, late); err != nil || n != 1 {
		t.Fatalf("reclaim: n=%d err=%v", n, err)
	}
	it, err := claimNext(db, late, time.Minute)
	if err != nil || it == nil || it.ID != next {
		t.Fatalf("dedupe row must be skipped over, want %d got %+v err=%v", next, it, err)
	}
	st, reason, _, leased := qState(t, db, dup)
	if st != "done" || reason != "dedupe" || leased {
		t.Errorf("dedupe row: state=%s reason=%s leased=%v", st, reason, leased)
	}
}

func TestMemdQueueFinish(t *testing.T) {
	db, _ := openTestMemd(t)
	for _, st := range []string{"done", "rejected", "unknown", "queued"} {
		id := qAdd(t, db, "s", "t"+st, "Edit", `{}`)
		if _, err := claimNext(db, qNow, time.Minute); err != nil {
			t.Fatal(err)
		}
		if ok, err := finish(db, id, 1, st, "why"); err != nil || !ok {
			t.Fatalf("finish %s: ok=%v err=%v", st, ok, err)
		}
		got, reason, _, leased := qState(t, db, id)
		if got != st || reason != "why" || leased {
			t.Errorf("%s: state=%s reason=%s leased=%v", st, got, reason, leased)
		}
	}
	id := qAdd(t, db, "s", "tbad", "Edit", `{}`)
	for _, bad := range []string{"claimed", "bogus", ""} {
		if _, err := finish(db, id, 1, bad, ""); err == nil {
			t.Errorf("finish(%q) must error", bad)
		}
	}
}

// R15: finish only updates a claimed row whose attempts match the claim.
func TestMemdQueueFinishGuard(t *testing.T) {
	db, _ := openTestMemd(t)
	id := qAdd(t, db, "s", "t1", "Edit", `{}`)
	it, err := claimNext(db, qNow, time.Minute)
	if err != nil || it == nil {
		t.Fatal(err)
	}
	if ok, err := finish(db, id, it.Attempts+1, "done", "stale"); err != nil || ok {
		t.Errorf("wrong attempts: ok=%v err=%v, want false/nil", ok, err)
	}
	if st, _, _, _ := qState(t, db, id); st != "claimed" {
		t.Errorf("state = %s, want claimed", st)
	}
	if ok, _ := finish(db, id, it.Attempts, "done", ""); !ok {
		t.Fatal("matching finish must succeed")
	}
	if ok, _ := finish(db, id, it.Attempts, "rejected", "again"); ok {
		t.Error("finish on a non-claimed row must match nothing")
	}
}

func TestMemdQueueRecordLoss(t *testing.T) {
	db, _ := openTestMemd(t)
	if err := recordLoss(db, "truncated", "input", 3); err != nil {
		t.Fatal(err)
	}
	var kind, detail string
	var n int
	if err := db.QueryRow(`SELECT kind, detail, n FROM loss_receipts`).Scan(&kind, &detail, &n); err != nil {
		t.Fatal(err)
	}
	if kind != "truncated" || detail != "input" || n != 3 {
		t.Errorf("got %s %s %d", kind, detail, n)
	}
}

// Real concurrent claimers on one *sql.DB: every row is claimed exactly once. DuckDB's
// optimistic concurrency rejects the loser of a race with a conflict error, which claimNext
// retries; the counter proves that path ran.
func TestMemdQueueConcurrentClaimers(t *testing.T) {
	db, _ := openTestMemd(t)
	const rows, workers = 50, 8
	for i := 0; i < rows; i++ {
		qAdd(t, db, "s", fmt.Sprintf("t%03d", i), "Edit", `{}`)
	}
	atomic.StoreInt64(&claimConflicts, 0)
	var mu sync.Mutex
	seen := map[int64]int{}
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				it, err := claimNext(db, qNow, time.Minute)
				if err != nil {
					errs <- err
					return
				}
				if it == nil {
					return
				}
				mu.Lock()
				seen[it.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("claimNext: %v", err)
	}
	if len(seen) != rows {
		t.Errorf("want %d distinct rows claimed, got %d", rows, len(seen))
	}
	for id, c := range seen {
		if c != 1 {
			t.Errorf("row %d claimed %d times", id, c)
		}
	}
	t.Logf("conflict retries: %d", atomic.LoadInt64(&claimConflicts))
	if atomic.LoadInt64(&claimConflicts) == 0 {
		t.Error("expected at least one conflict retry with 8 claimers; retry path unexercised")
	}
}
