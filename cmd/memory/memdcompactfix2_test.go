package main

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The output cap also applies when numeric ids match nothing and only summaries come back.
func TestGetObservationsSummariesWithMissingNumericIDsAreCapped(t *testing.T) {
	db, _ := openTestMemd(t)
	var ids []any
	for i := 1; i <= 4; i++ {
		text := strings.Repeat("quail"+strconv.Itoa(i)+" ", 38000)
		if _, err := storeCompaction(db, &compactRecord{SessionID: "s-m-" + strconv.Itoa(i), Trigger: "auto", Cwd: t.TempDir(), Summary: text}, time.Now()); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, "S"+strconv.Itoa(i))
	}
	out, err := memGetObservations(db, map[string]any{"ids": append(ids, float64(999999))})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > memdCapObservations {
		t.Fatalf("output is %d bytes, cap is %d", len(out), memdCapObservations)
	}
}

// The compact-context exclusion is exact: only source=compact, only compaction summaries, and
// the HTTP route carries session_id through.
func TestStartContextExclusionIsNarrow(t *testing.T) {
	db := ctxDB(t)
	project := projectKey("/w/narrow")
	if _, err := storeCompaction(db, &compactRecord{SessionID: "sess-own", Trigger: "auto", Cwd: "/w/narrow", Summary: "OWNCOMPACTMARKER narwhal"}, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	ctxSum(t, db, 900, project, "Own model summary", "MODELSUMMARYMARKER", time.Now().Add(-30*time.Second))
	ctxExec(t, db, `UPDATE summaries SET session_id = 'sess-own' WHERE id = 900`)
	for _, src := range []string{"startup", "resume", "clear", ""} {
		if got := buildStartContextFor(db, project, src, "sess-own", time.Now()); !strings.Contains(got, "Context compaction") {
			t.Errorf("source %q hid the session's compaction:\n%s", src, got)
		}
	}
	got := buildStartContextFor(db, project, "compact", "sess-own", time.Now())
	if strings.Contains(got, "Context compaction") {
		t.Errorf("compact source kept the compaction row:\n%s", got)
	}
	if !strings.Contains(got, "Own model summary") {
		t.Errorf("the session's model-written summary was dropped:\n%s", got)
	}
	h := memdHandler(db)
	base := "/context?project=" + url.QueryEscape(project) + "&source=compact"
	_, withSID, _ := httpDo(t, h, "GET", base+"&session_id=sess-own", "")
	_, noSID, _ := httpDo(t, h, "GET", base, "")
	if strings.Contains(withSID, "Context compaction") || !strings.Contains(noSID, "Context compaction") {
		t.Errorf("HTTP /context ignores session_id:\nwith: %s\nwithout: %s", withSID, noSID)
	}
}

// A capture set aside six days ago is still kept.
func TestDrainKeepsRecentRejected(t *testing.T) {
	db, _ := openTestMemd(t)
	spool := filepath.Join(t.TempDir(), "compactions")
	rej := filepath.Join(spool, "rejected")
	if err := os.MkdirAll(rej, 0o700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(rej, "six.json")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-6 * 24 * time.Hour)
	if err := os.Chtimes(f, past, past); err != nil {
		t.Fatal(err)
	}
	if _, err := drainCompactions(context.Background(), db, spool); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f); err != nil {
		t.Fatalf("six-day-old rejected entry was pruned: %v", err)
	}
}
