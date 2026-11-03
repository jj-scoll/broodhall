package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fastWorkers shrinks the worker timing so a retry cycle takes milliseconds. Tests using it
// must not run in parallel (the knobs are package vars).
func fastWorkers(t *testing.T) {
	t.Helper()
	ob, om, op := observerBackoffBase, observerBackoffMax, observerPoll
	observerBackoffBase, observerBackoffMax, observerPoll = 5*time.Millisecond, 20*time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { observerBackoffBase, observerBackoffMax, observerPoll = ob, om, op })
}

// startWorkers runs n workers and returns an idempotent stop that waits for them to exit.
func startWorkers(t *testing.T, db *sql.DB, n int) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runObserverWorkers(ctx, db, n); close(done) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("workers did not exit after cancel")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func countQ(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// scriptedClaude is a thread-safe runClaude fake. mode picks the reply; calls are counted
// per stdin (one stdin per distinct tool use payload).
type scriptedClaude struct {
	mu       sync.Mutex
	perStdin map[string]int
	total    int
	reply    func(call int, stdin string) fakeReply
	onCall   func(stdin string)
}

func newScripted(t *testing.T, reply func(call int, stdin string) fakeReply) *scriptedClaude {
	t.Helper()
	s := &scriptedClaude{perStdin: map[string]int{}, reply: reply}
	old := runClaude
	t.Cleanup(func() { runClaude = old })
	runClaude = func(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
		s.mu.Lock()
		s.perStdin[string(stdin)]++
		s.total++
		n := s.total
		hook := s.onCall
		s.mu.Unlock()
		if hook != nil {
			hook(string(stdin))
		}
		r := s.reply(n, string(stdin))
		return []byte(r.out), r.err
	}
	return s
}

func (s *scriptedClaude) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

var notLoggedIn = fakeReply{out: `{"type":"result","is_error":true,"result":"Not logged in - please run /login"}`}

func addRows(t *testing.T, db *sql.DB, n int) []int64 {
	t.Helper()
	var ids []int64
	for i := 0; i < n; i++ {
		ids = append(ids, qAdd(t, db, "sess", fmt.Sprintf("tu%d", i), "Edit", fmt.Sprintf(`{"file_path":"src/f%d.go"}`, i)))
	}
	return ids
}

// Brief step 1: a sign-out loses nothing and recovery drains the queue with one call per key.
func TestMemdWorkerSignOutRecovery(t *testing.T) {
	fastWorkers(t)
	db, _ := openTestMemd(t)
	ids := addRows(t, db, 20)
	var signedOut atomic.Bool
	signedOut.Store(true)
	sc := newScripted(t, func(call int, stdin string) fakeReply {
		if signedOut.Load() {
			return notLoggedIn
		}
		return result(t, goodObs(), "")
	})
	stop := startWorkers(t, db, 2)

	waitFor(t, "every row attempted while signed out", func() bool { return sc.calls() >= 20 })
	waitFor(t, "signed-out health", func() bool { so, _, _ := observerState(db); return so })
	// Stop so no row is mid-call when counting; in-flight rows are handed back on shutdown.
	stop()
	so, since, waiting := observerState(db)
	if !so || waiting != 20 || since.IsZero() {
		t.Fatalf("observerState = %v, %v, %d; want signed out, nonzero since, 20 waiting", so, since, waiting)
	}
	if n := countQ(t, db, `SELECT count(*) FROM queue WHERE state IN ('queued','unknown','claimed')`); n != 20 {
		t.Fatalf("rows still in the queue = %d, want 20", n)
	}
	if n := countQ(t, db, `SELECT count(*) FROM observations`) + countQ(t, db, `SELECT count(*) FROM loss_receipts`); n != 0 {
		t.Fatalf("observations+losses while signed out = %d, want 0", n)
	}

	// Count only successful calls from here: reset per-stdin counters at the flip.
	sc.mu.Lock()
	sc.perStdin = map[string]int{}
	sc.mu.Unlock()
	signedOut.Store(false)
	stop = startWorkers(t, db, 2)

	waitFor(t, "queue drained", func() bool {
		return countQ(t, db, `SELECT count(*) FROM queue WHERE state = 'done'`) == 20
	})
	stop()
	for _, id := range ids {
		if st, _, _, _ := qState(t, db, id); st != "done" {
			t.Errorf("row %d state = %s", id, st)
		}
	}
	if n := countQ(t, db, `SELECT count(*) FROM observations WHERE source = 'live'`); n != 20 {
		t.Errorf("live observations = %d, want 20", n)
	}
	sc.mu.Lock()
	for stdin, n := range sc.perStdin {
		if n != 1 {
			t.Errorf("calls for one key after recovery = %d, want 1 (%.40q)", n, stdin)
		}
	}
	if len(sc.perStdin) != 20 {
		t.Errorf("distinct payloads called = %d, want 20", len(sc.perStdin))
	}
	sc.mu.Unlock()
	so, _, waiting = observerState(db)
	if so || waiting != 0 {
		t.Errorf("after recovery signedOut=%v waiting=%d", so, waiting)
	}
	if n := countQ(t, db, `SELECT count(*) FROM observer_health WHERE last_outcome = 'done'`); n != 1 {
		t.Error("health must show done after recovery")
	}
}

func TestMemdWorkerStoresLiveObservation(t *testing.T) {
	fastWorkers(t)
	db, _ := openTestMemd(t)
	if _, err := db.Exec(`INSERT INTO sessions (session_id, project) VALUES ('sess', 'proj-x')`); err != nil {
		t.Fatal(err)
	}
	id := qAdd(t, db, "sess", "tu1", "Edit", `{"file_path":"src/parser.go","new_string":"if tok == nil"}`)
	if _, err := db.Exec(`UPDATE tool_uses SET output = 'The file src/parser.go has been updated. parser.go now checks the token for nil.'`); err != nil {
		t.Fatal(err)
	}
	m := goodObs()
	m["files_read"] = []string{"src/not_in_payload.go"} // dropped by the anchor check
	newScripted(t, func(int, string) fakeReply {
		return result(t, m, `{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100,"cache_creation_input_tokens":20}`)
	})
	startWorkers(t, db, 2)
	waitFor(t, "done", func() bool { st, _, _, _ := qState(t, db, id); return st == "done" })

	var source, pv, model, key, project, sess, facts string
	if err := db.QueryRow(`SELECT source, prompt_version, generated_by_model, dedupe_key, project, session_id, facts FROM observations`).
		Scan(&source, &pv, &model, &key, &project, &sess, &facts); err != nil {
		t.Fatal(err)
	}
	it := &queueItem{ToolName: "Edit", Input: `{"file_path":"src/parser.go","new_string":"if tok == nil"}`,
		Output: "The file src/parser.go has been updated. parser.go now checks the token for nil."}
	payload, _ := observerPayload(it)
	if source != "live" || pv != observerPromptVersion || model != observerModel || project != "proj-x" || sess != "sess" ||
		key != dedupeKey(observerPromptVersion, observerModel, payload) {
		t.Errorf("row = %s %s %s %s %s %s", source, pv, model, project, sess, key)
	}
	if !strings.Contains(facts, "parser.go") {
		t.Errorf("facts = %s", facts)
	}
	if n := countQ(t, db, `SELECT count(*) FROM observation_files WHERE path = 'src/parser.go' AND kind = 'modified'`); n != 1 {
		t.Errorf("modified file rows = %d", n)
	}
	// The write path indexed the observation itself (read the postings, not a search that would catch up).
	// The hook runs just after the settle commits, so allow it a moment.
	var nIdx int
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if nIdx = countQ(t, db, `SELECT count(*) FROM ox_docs`); nIdx == 1 {
			break
		}
	}
	if nIdx != 1 {
		t.Errorf("observation not in the search postings after the worker stored it (ox_docs rows = %d)", nIdx)
	}
	var qkey string
	var in, cost = int64(0), 0.0
	if err := db.QueryRow(`SELECT dedupe_key, input_tokens, cost_usd FROM queue WHERE id = ?`, id).Scan(&qkey, &in, &cost); err != nil {
		t.Fatal(err)
	}
	if qkey != key || in != 10 || cost <= 0 {
		t.Errorf("queue key=%q in=%d cost=%v", qkey, in, cost)
	}
	var detail string
	var n int
	if err := db.QueryRow(`SELECT detail, n FROM loss_receipts WHERE kind = 'anchor_drop'`).Scan(&detail, &n); err != nil {
		t.Fatalf("anchor_drop receipt: %v", err)
	}
	if n != 1 || strings.Contains(detail, "not_in_payload") {
		t.Errorf("anchor_drop detail=%q n=%d (must be count-only)", detail, n)
	}
}

func TestMemdWorkerRejected(t *testing.T) {
	fastWorkers(t)
	db, _ := openTestMemd(t)
	id := addRows(t, db, 1)[0]
	bad := goodObs()
	bad["title"] = ""
	sc := newScripted(t, func(int, string) fakeReply { return result(t, bad, "") })
	startWorkers(t, db, 2)
	waitFor(t, "rejected", func() bool { st, _, _, _ := qState(t, db, id); return st == "rejected" })
	_, reason, _, leased := qState(t, db, id)
	if reason != "empty_title" || leased {
		t.Errorf("reason=%q leased=%v", reason, leased)
	}
	if n := countQ(t, db, `SELECT count(*) FROM loss_receipts WHERE kind = 'rejected' AND detail = 'empty_title'`); n != 1 {
		t.Errorf("rejected loss receipts = %d", n)
	}
	if n := countQ(t, db, `SELECT count(*) FROM observations`); n != 0 {
		t.Errorf("observations = %d, want 0", n)
	}
	if sc.calls() != 2 {
		t.Errorf("calls = %d, want 2 (one retry)", sc.calls())
	}
}

// R15: a worker that lost its lease must not write the observation, a loss receipt or a
// health row, whatever the outcome of its call.
func TestMemdWorkerLostLease(t *testing.T) {
	bad := goodObs()
	bad["title"] = ""
	cases := []struct {
		name  string
		reply fakeReply
	}{
		{"done", result(t, goodObs(), "")},
		{"rejected", result(t, bad, "")},
		{"unknown", notLoggedIn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fastWorkers(t)
			db, _ := openTestMemd(t)
			id := addRows(t, db, 1)[0]
			var once sync.Once
			sc := newScripted(t, func(int, string) fakeReply { return tc.reply })
			sc.onCall = func(string) {
				once.Do(func() {
					// Simulate expiry + re-claim by another worker: same row, higher attempts.
					if _, err := db.Exec(`UPDATE queue SET attempts = attempts + 1 WHERE id = ?`, id); err != nil {
						t.Error(err)
					}
				})
			}
			processed := make(chan struct{}, 8)
			old := workerAfterItem
			workerAfterItem = func() { processed <- struct{}{} }
			t.Cleanup(func() { workerAfterItem = old })
			startWorkers(t, db, 1)
			select {
			case <-processed:
			case <-time.After(10 * time.Second):
				t.Fatal("item never processed")
			}
			for _, q := range []string{
				`SELECT count(*) FROM observations`,
				`SELECT count(*) FROM observation_files`,
				`SELECT count(*) FROM loss_receipts`,
				`SELECT count(*) FROM observer_health`,
			} {
				if n := countQ(t, db, q); n != 0 {
					t.Errorf("%s = %d, want 0 for a lost lease", q, n)
				}
			}
			if st, _, att, _ := qState(t, db, id); st != "claimed" || att != 2 {
				t.Errorf("row state=%s attempts=%d, want untouched claimed/2", st, att)
			}
		})
	}
}

// R31: two rows with an identical payload get exactly one observer call and one observation.
func TestMemdWorkerDuplicatePayloadOneCall(t *testing.T) {
	fastWorkers(t)
	db, _ := openTestMemd(t)
	var ids []int64
	for i := 0; i < 2; i++ {
		ids = append(ids, qAdd(t, db, "sess", fmt.Sprintf("dup%d", i), "Edit", `{"file_path":"src/same.go"}`))
	}
	sc := newScripted(t, func(int, string) fakeReply { return result(t, goodObs(), "") })
	sc.onCall = func(string) { time.Sleep(80 * time.Millisecond) } // keep the first in flight
	startWorkers(t, db, 2)
	waitFor(t, "both done", func() bool {
		return countQ(t, db, `SELECT count(*) FROM queue WHERE state = 'done'`) == 2
	})
	if sc.calls() != 1 {
		t.Errorf("observer calls = %d, want 1", sc.calls())
	}
	if n := countQ(t, db, `SELECT count(*) FROM observations`); n != 1 {
		t.Errorf("observations = %d, want 1", n)
	}
	reasons := 0
	for _, id := range ids {
		if _, r, _, _ := qState(t, db, id); r == "dedupe" {
			reasons++
		}
	}
	if reasons != 1 {
		t.Errorf("rows finished as dedupe = %d, want 1", reasons)
	}
}

// R25: CLI-answered failures are capped at 5 attempts; the row is rejected with a receipt.
func TestMemdWorkerCapMissingStructuredOutput(t *testing.T) {
	fastWorkers(t)
	db, _ := openTestMemd(t)
	id := addRows(t, db, 1)[0]
	sc := newScripted(t, func(int, string) fakeReply {
		return fakeReply{out: `{"type":"result","is_error":false,"result":"text","usage":{"input_tokens":3}}`}
	})
	startWorkers(t, db, 2)
	waitFor(t, "rejected", func() bool { st, _, _, _ := qState(t, db, id); return st == "rejected" })
	_, reason, att, _ := qState(t, db, id)
	if reason != "attempts_exhausted" || att != observerMaxAttempts {
		t.Errorf("reason=%q attempts=%d", reason, att)
	}
	time.Sleep(50 * time.Millisecond)
	if sc.calls() != observerMaxAttempts {
		t.Errorf("calls = %d, want %d", sc.calls(), observerMaxAttempts)
	}
	if n := countQ(t, db, `SELECT count(*) FROM loss_receipts WHERE kind = 'attempts_exhausted'`); n != 1 {
		t.Errorf("attempts_exhausted receipts = %d", n)
	}
	var in int64
	if err := db.QueryRow(`SELECT input_tokens FROM queue WHERE id = ?`, id).Scan(&in); err != nil || in != 3*observerMaxAttempts {
		t.Errorf("usage must accumulate per attempt: input_tokens=%d err=%v", in, err)
	}
}

// R25: auth and transport unknowns are never capped.
func TestMemdWorkerAuthNeverCapped(t *testing.T) {
	fastWorkers(t)
	db, _ := openTestMemd(t)
	id := addRows(t, db, 1)[0]
	sc := newScripted(t, func(int, string) fakeReply { return notLoggedIn })
	startWorkers(t, db, 2)
	waitFor(t, "more calls than the cap", func() bool { return sc.calls() > observerMaxAttempts+3 })
	st, _, att, _ := qState(t, db, id)
	if st == "rejected" || st == "done" || att <= observerMaxAttempts {
		t.Errorf("state=%s attempts=%d; auth unknown must stay in the queue past the cap", st, att)
	}
	if n := countQ(t, db, `SELECT count(*) FROM loss_receipts`); n != 0 {
		t.Errorf("loss receipts = %d, want 0", n)
	}
}

// R28: a row whose key already has an observation finishes as dedupe with no observer call.
func TestMemdWorkerDedupeSkipsObserver(t *testing.T) {
	fastWorkers(t)
	db, _ := openTestMemd(t)
	id := addRows(t, db, 1)[0]
	it := &queueItem{ToolName: "Edit", Input: `{"file_path":"src/f0.go"}`, Output: "out"}
	payload, _ := observerPayload(it)
	if _, err := db.Exec(`INSERT INTO observations (dedupe_key, source) VALUES (?, 'live')`,
		dedupeKey(observerPromptVersion, observerModel, payload)); err != nil {
		t.Fatal(err)
	}
	sc := newScripted(t, func(int, string) fakeReply { return result(t, goodObs(), "") })
	startWorkers(t, db, 2)
	waitFor(t, "done", func() bool { st, _, _, _ := qState(t, db, id); return st == "done" })
	if _, reason, _, _ := qState(t, db, id); reason != "dedupe" {
		t.Errorf("reason = %q", reason)
	}
	if sc.calls() != 0 {
		t.Errorf("observer calls = %d, want 0", sc.calls())
	}
	if n := countQ(t, db, `SELECT count(*) FROM observations`); n != 1 {
		t.Errorf("observations = %d, want 1", n)
	}
}

func TestMemdWorkerBackoff(t *testing.T) {
	tests := []struct {
		attempts int
		want     time.Duration
	}{
		{1, 30 * time.Second}, {2, time.Minute}, {3, 2 * time.Minute}, {5, 8 * time.Minute}, {6, 15 * time.Minute},
		{7, 15 * time.Minute}, {100, 15 * time.Minute}, {0, 30 * time.Second},
	}
	for _, tt := range tests {
		if got := observerBackoff(tt.attempts); got != tt.want {
			t.Errorf("observerBackoff(%d) = %v, want %v", tt.attempts, got, tt.want)
		}
	}
}

func TestMemdWorkerExitsOnCancel(t *testing.T) {
	fastWorkers(t)
	db, _ := openTestMemd(t)
	stop := startWorkers(t, db, 3)
	time.Sleep(20 * time.Millisecond)
	stop() // fails the test itself if the workers do not exit
}
