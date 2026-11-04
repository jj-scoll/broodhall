package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// F1: summary-only get_observations obeys the same output cap as every other id mix.
func TestGetObservationsSummaryOnlyIsCapped(t *testing.T) {
	db, _ := openTestMemd(t)
	var ids []any
	for i := 1; i <= 4; i++ {
		text := strings.Repeat("quail"+strconv.Itoa(i)+" ", 38000) // about 190 KB each
		r := &compactRecord{SessionID: "s-big-" + strconv.Itoa(i), Trigger: "auto", Cwd: t.TempDir(), Summary: text}
		if _, err := storeCompaction(db, r, time.Now()); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, "S"+strconv.Itoa(i))
	}
	out, err := memGetObservations(db, map[string]any{"ids": ids})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > memdCapObservations {
		t.Fatalf("summary-only output is %d bytes, cap is %d", len(out), memdCapObservations)
	}
	if !strings.Contains(out, "Output trimmed") {
		t.Fatalf("no trim marker in capped output")
	}
}

// F2: only the top level of the spool is ingested; rejected/ is never read or shown, and
// entries there older than 7 days are pruned on drain.
func TestDrainNeverIngestsRejectedAndPrunesOld(t *testing.T) {
	db, _ := openTestMemd(t)
	spool := filepath.Join(t.TempDir(), "compactions")
	rej := filepath.Join(spool, "rejected")
	if err := os.MkdirAll(rej, 0o700); err != nil {
		t.Fatal(err)
	}
	valid := compactPayload("sess-rej", "/w/rej", "auto", "walrus secret ghp_abcdefghijklmnopqrstuvwxyz0123456789")
	oldF, newF := filepath.Join(rej, "old.json"), filepath.Join(rej, "new.json")
	for _, f := range []string{oldF, newF} {
		if err := os.WriteFile(f, []byte(valid), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(oldF, past, past); err != nil {
		t.Fatal(err)
	}
	n, err := drainCompactions(context.Background(), db, spool)
	if err != nil || n != 0 {
		t.Fatalf("drain n=%d err=%v", n, err)
	}
	if got := countRows(t, db, `SELECT count(*) FROM summaries`); got != 0 {
		t.Fatalf("rejected/ content was ingested (%d rows)", got)
	}
	if _, err := os.Stat(oldF); !os.IsNotExist(err) {
		t.Fatalf("rejected entry older than 7 days not pruned (stat err %v)", err)
	}
	if _, err := os.Stat(newF); err != nil {
		t.Fatalf("recent rejected entry was removed: %v", err)
	}
	if res, _ := memSearch(db, map[string]any{"query": "walrus"}); strings.Contains(res, "#S") {
		t.Fatalf("rejected text is searchable:\n%s", res)
	}
}

// A capture set aside just now starts its 7 days then, not at the age of the file.
func TestSetAsideRestartsRejectedClock(t *testing.T) {
	db, _ := openTestMemd(t)
	spool := filepath.Join(t.TempDir(), "compactions")
	if err := os.MkdirAll(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(spool, "bad-1.json")
	if err := os.WriteFile(f, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(f, past, past); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := drainCompactions(context.Background(), db, spool); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(spool, "rejected", "bad-1.json")); err != nil {
		t.Fatalf("freshly set-aside file was pruned at once: %v", err)
	}
}

// F3: stale .tmp spool files go; a fresh one (a hook mid-write) stays.
func TestDrainRemovesStaleTmp(t *testing.T) {
	db, _ := openTestMemd(t)
	spool := filepath.Join(t.TempDir(), "compactions")
	if err := os.MkdirAll(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	stale, fresh := filepath.Join(spool, ".a-1.json.tmp"), filepath.Join(spool, ".b-2.json.tmp")
	for _, f := range []string{stale, fresh} {
		if err := os.WriteFile(f, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatal(err)
	}
	if _, err := drainCompactions(context.Background(), db, spool); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale tmp not removed (stat err %v)", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh tmp removed: %v", err)
	}
}

// F4: a cancelled context stops the pass between files.
func TestDrainStopsWhenContextCancelled(t *testing.T) {
	db, _ := openTestMemd(t)
	spool := filepath.Join(t.TempDir(), "compactions")
	for i := 0; i < 5; i++ {
		in := compactPayload("s-c"+strconv.Itoa(i), "/w/c", "auto", "cancel text "+strconv.Itoa(i))
		if err := memCompactCapture(strings.NewReader(in), spool, time.Now().Add(time.Duration(i)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n, err := drainCompactions(ctx, db, spool)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || len(spoolFiles(t, spool)) != 5 {
		t.Fatalf("cancelled drain ingested %d, left %d files", n, len(spoolFiles(t, spool)))
	}
	// the next pass (live context) finishes the work
	if n, _ := drainCompactions(context.Background(), db, spool); n != 5 {
		t.Fatalf("follow-up drain ingested %d, want 5", n)
	}
}

// F5: the hook records CLAUDE_PROJECT_DIR and the daemon files the summary under it, the way
// memContext and memdLookupFile pick the project; cwd is the fallback.
func TestCompactionProjectPrefersClaudeProjectDir(t *testing.T) {
	db, _ := openTestMemd(t)
	base := t.TempDir()
	projDir, otherDir := filepath.Join(base, "realproj"), filepath.Join(base, "movedto")
	for _, d := range []string{projDir, otherDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	spool := filepath.Join(t.TempDir(), "compactions")
	t.Setenv("CLAUDE_PROJECT_DIR", projDir)
	if err := memCompactCapture(strings.NewReader(compactPayload("s-pd", otherDir, "auto", "project dir text")), spool, time.Now()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	if err := memCompactCapture(strings.NewReader(compactPayload("s-nopd", otherDir, "auto", "no project dir text")), spool, time.Now().Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := drainCompactions(context.Background(), db, spool); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, db, `SELECT count(*) FROM summaries WHERE session_id = 's-pd' AND project = 'realproj'`); got != 1 {
		t.Fatalf("CLAUDE_PROJECT_DIR ignored: project rows = %d", got)
	}
	if got := countRows(t, db, `SELECT count(*) FROM summaries WHERE session_id = 's-nopd' AND project = 'movedto'`); got != 1 {
		t.Fatalf("cwd fallback broken: project rows = %d", got)
	}
}

// F6: after a compaction the session already holds its own summary; the SessionStart context
// (source=compact) must not show it again. Other sessions' compactions still show.
func TestStartContextOmitsOwnCompactionOnCompact(t *testing.T) {
	db := ctxDB(t)
	when := time.Now().Add(-time.Minute)
	for _, c := range []struct{ sid, text string }{
		{"sess-other", "OTHERCOMPACTMARKER walrus"},
		{"sess-own", "OWNCOMPACTMARKER narwhal"},
	} {
		if _, err := storeCompaction(db, &compactRecord{SessionID: c.sid, Trigger: "auto", Cwd: "/w/p", Summary: c.text}, when); err != nil {
			t.Fatal(err)
		}
		when = when.Add(time.Second)
	}
	project := projectKey("/w/p")
	got := buildStartContextFor(db, project, "compact", "sess-own", time.Now())
	if strings.Contains(got, "OWNCOMPACTMARKER") {
		t.Fatalf("own compaction summary re-injected on source=compact:\n%s", got)
	}
	if n := strings.Count(got, "Context compaction"); n != 1 {
		t.Fatalf("want exactly the other session's compaction row, got %d:\n%s", n, got)
	}
	// startup and resume of another session still see it.
	if got := buildStartContextFor(db, project, "startup", "sess-new", time.Now()); !strings.Contains(got, "OWNCOMPACTMARKER") && !strings.Contains(got, "OTHERCOMPACTMARKER") {
		t.Fatalf("startup context shows no compactions at all:\n%s", got)
	}
	// without a session id nothing is excluded
	if got := buildStartContextFor(db, project, "compact", "", time.Now()); !strings.Contains(got, "Context compaction") {
		t.Fatalf("no session id: compactions should show:\n%s", got)
	}
}

// F7: one S-id parser.
func TestParseSummaryID(t *testing.T) {
	for in, want := range map[string]int64{"S12": 12, "#S12": 12, " S7 ": 7, "s3": 3} {
		if got, ok := parseSummaryID(in); !ok || got != want {
			t.Errorf("parseSummaryID(%q) = %d,%v want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "S", "S0", "Sx", "S+5", "S-1", "S1.5", "12", "#12", "SS1", "S 1"} {
		if got, ok := parseSummaryID(in); ok {
			t.Errorf("parseSummaryID(%q) = %d, want rejection", in, got)
		}
	}
}

// F7: get_observations and timeline agree on what an S id is.
func TestSummaryIDsAgreeAcrossTools(t *testing.T) {
	db, _ := openTestMemd(t)
	if _, err := storeCompaction(db, &compactRecord{SessionID: "s-id", Trigger: "auto", Cwd: "/w/id", Summary: "agree text"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{"S1", "#S1", "s1", "S+1", "S01x"} {
		_, okParse := parseSummaryID(in)
		gotG, errG := memGetObservations(db, map[string]any{"ids": []any{in}})
		gotT, _ := memTimeline(db, map[string]any{"anchor": in})
		okGet := errG == nil && strings.Contains(gotG, "agree text")
		okTl := strings.Contains(gotT, "#S1") && !strings.Contains(gotT, "looks like S12")
		if okGet != okParse || okTl != okParse {
			t.Errorf("%q: parse=%v get=%v timeline=%v\nget: %v %s\ntimeline: %s", in, okParse, okGet, okTl, errG, gotG, gotT)
		}
	}
}
