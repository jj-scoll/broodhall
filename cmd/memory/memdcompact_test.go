package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func compactPayload(sid, cwd, trigger, summary string) string {
	b, _ := json.Marshal(map[string]any{
		"session_id": sid, "transcript_path": "/x/" + sid + ".jsonl", "cwd": cwd,
		"hook_event_name": "PostCompact", "trigger": trigger, "compact_summary": summary,
	})
	return string(b)
}

func spoolFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	return m
}

func TestCompactCaptureSpoolsRedactedRecord(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "compactions")
	secret := "AKIAIOSFODNN7EXAMPLE"
	err := memCompactCapture(strings.NewReader(compactPayload("sess-1", "/work/app", "manual",
		"We fixed the quokka parser. key="+secret)), spool, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	files := spoolFiles(t, spool)
	if len(files) != 1 {
		t.Fatalf("want 1 spool file, got %v", files)
	}
	b, _ := os.ReadFile(files[0])
	if bytes.Contains(b, []byte(secret)) {
		t.Fatalf("spool file holds an unredacted secret: %s", b)
	}
	if !bytes.Contains(b, []byte("quokka parser")) {
		t.Fatalf("summary text missing: %s", b)
	}
	if left, _ := filepath.Glob(filepath.Join(spool, ".*")); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

func TestCompactCaptureRejectsBadInput(t *testing.T) {
	for name, in := range map[string]string{
		"unsafe session": compactPayload("../evil", "/w", "auto", "s"),
		"no session":     compactPayload("", "/w", "auto", "s"),
		"no summary":     compactPayload("sess-1", "/w", "auto", ""),
		"not json":       "nope",
	} {
		spool := filepath.Join(t.TempDir(), "compactions")
		if err := memCompactCapture(strings.NewReader(in), spool, time.Now()); err == nil {
			t.Errorf("%s: want error", name)
		}
		if f := spoolFiles(t, spool); len(f) != 0 {
			t.Errorf("%s: wrote %v", name, f)
		}
	}
}

// The hook never fails the session, even on garbage; its only output is a stderr note.
func TestCompactCaptureCmdFailsOpen(t *testing.T) {
	t.Setenv("MEMORY_DIR", t.TempDir())
	var errOut bytes.Buffer
	if rc := memCompactCaptureCmd(strings.NewReader("garbage"), &errOut); rc != 0 || errOut.Len() == 0 {
		t.Fatalf("rc=%d stderr=%q", rc, errOut.String())
	}
	errOut.Reset()
	if rc := memCompactCaptureCmd(strings.NewReader(compactPayload("s1", "/w", "auto", "ok")), &errOut); rc != 0 || errOut.Len() != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, errOut.String())
	}
	if len(spoolFiles(t, filepath.Join(memdDir(), "compactions"))) != 1 {
		t.Fatal("valid payload was not spooled under the state dir")
	}
}

func countRows(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCompactionDrainIngestsSearchableIdempotent(t *testing.T) {
	db, _ := openTestMemd(t)
	proj := filepath.Join(t.TempDir(), "myproj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	spool := filepath.Join(t.TempDir(), "compactions")
	when := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	in := compactPayload("sess-9", proj, "auto", "Decided to shard the zebrafish cache by tenant.")
	if err := memCompactCapture(strings.NewReader(in), spool, when); err != nil {
		t.Fatal(err)
	}
	n, err := drainCompactions(db, spool)
	if err != nil || n != 1 {
		t.Fatalf("drain n=%d err=%v", n, err)
	}
	if f := spoolFiles(t, spool); len(f) != 0 {
		t.Fatalf("spool not emptied: %v", f)
	}
	if got := countRows(t, db, `SELECT count(*) FROM summaries WHERE project = 'myproj' AND session_id = 'sess-9'`); got != 1 {
		t.Fatalf("summaries rows = %d", got)
	}
	// Same capture again (hook retried, or a crash before the file was removed): no duplicate.
	if err := memCompactCapture(strings.NewReader(in), spool, when.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := drainCompactions(db, spool); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, db, `SELECT count(*) FROM summaries`); got != 1 {
		t.Fatalf("after re-drain summaries = %d, want 1", got)
	}
	if got := countRows(t, db, `SELECT count(*) FROM compactions`); got != 1 {
		t.Fatalf("compactions = %d, want 1", got)
	}
	if f := spoolFiles(t, spool); len(f) != 0 {
		t.Fatalf("duplicate not cleared from spool: %v", f)
	}

	got, err := memSearch(db, map[string]any{"query": "zebrafish"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "#S1") || !strings.Contains(got, "ompaction") {
		t.Fatalf("search did not surface the compaction summary:\n%s", got)
	}
	if got, _ := memSearch(db, map[string]any{"query": "zebrafish", "project": "other"}); strings.Contains(got, "#S1") {
		t.Fatalf("project filter ignored:\n%s", got)
	}
	read, err := memGetObservations(db, map[string]any{"ids": []any{"S1"}})
	if err != nil || !strings.Contains(read, "shard the zebrafish cache") {
		t.Fatalf("get S1: %v\n%s", err, read)
	}
	tl, err := memTimeline(db, map[string]any{"anchor": "S1"})
	if err != nil || !strings.Contains(tl, "#S1") {
		t.Fatalf("timeline S1: %v\n%s", err, tl)
	}
}

// A raw hook payload dropped by run.sh (no binary yet) is ingested too; garbage is set aside.
func TestCompactionDrainRawPayloadAndGarbage(t *testing.T) {
	db, _ := openTestMemd(t)
	spool := filepath.Join(t.TempDir(), "compactions")
	os.MkdirAll(spool, 0o755)
	os.WriteFile(filepath.Join(spool, "raw-1.json"),
		[]byte(compactPayload("sess-r", "/nonexistent/rawproj", "manual", "raw okapi summary key=AKIAIOSFODNN7EXAMPLE")), 0o600)
	os.WriteFile(filepath.Join(spool, "bad.json"), []byte("{not json"), 0o600)
	n, err := drainCompactions(db, spool)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	var notes string
	if err := db.QueryRow(`SELECT learned FROM summaries WHERE session_id = 'sess-r'`).Scan(&notes); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(notes, "AKIAIOSFODNN7EXAMPLE") || !strings.Contains(notes, "okapi") {
		t.Fatalf("stored text not redacted or missing: %q", notes)
	}
	if f := spoolFiles(t, spool); len(f) != 0 {
		t.Fatalf("spool left: %v", f)
	}
	if rej, _ := filepath.Glob(filepath.Join(spool, "rejected", "*")); len(rej) != 1 {
		t.Fatalf("garbage not set aside: %v", rej)
	}
}

func TestRunCompactionDrainPicksUpLaterCaptures(t *testing.T) {
	db, _ := openTestMemd(t)
	old := compactDrainPoll
	compactDrainPoll = 20 * time.Millisecond
	defer func() { compactDrainPoll = old }()
	spool := filepath.Join(t.TempDir(), "compactions")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runCompactionDrain(ctx, db, spool); close(done) }()
	defer func() { cancel(); <-done }()
	if err := memCompactCapture(strings.NewReader(compactPayload("s-late", "/w/late", "auto", "late narwhal text")), spool, time.Now()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if countRows(t, db, `SELECT count(*) FROM summaries WHERE session_id = 's-late'`) == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("daemon loop never ingested the capture")
}

// An existing database (no compactions table, as written by earlier releases) migrates in place.
func TestCompactionsMigrationInPlace(t *testing.T) {
	db, dir := openTestMemd(t)
	if _, err := db.Exec(`INSERT INTO summaries (project, session_id, request, source) VALUES ('p','s','old','live')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE compactions`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db2, err := openMemdDB(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	if got := countRows(t, db2, `SELECT count(*) FROM summaries WHERE request = 'old'`); got != 1 {
		t.Fatalf("old row lost: %d", got)
	}
	if got := countRows(t, db2, `SELECT count(*) FROM compactions`); got != 0 {
		t.Fatalf("compactions = %d", got)
	}
}
