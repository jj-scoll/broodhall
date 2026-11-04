package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Coverage the independent verifier found missing (mutations of the core change that the
// first tests did not catch).

var compactSecrets = []string{
	memdFakeGH, "aws_secret_access_key=" + memdFakeAWS, memdFakeOAI, memdFakePEM,
	"AKIAIOSFODNN7EXAMPLE", "password=hunter2hunter2", `"api_key": "` + memdFakeOAI + `"`,
	"token: " + memdFakeGH + "; secret=" + memdFakeOAI,
	"postgres://user:p4ssw0rdXYZ@db.example.com:5432/app",
}

// A raw run.sh copy and the binary's masked copy of one compaction are one row: the dedupe key
// comes from the masked text, so masking must be a fixed point.
func TestRawAndHookCopyAreOneRow(t *testing.T) {
	for i, sec := range compactSecrets {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			db, _ := openTestMemd(t)
			spool := filepath.Join(t.TempDir(), "compactions")
			payload := compactPayload("sess-dup", "/nonexistent/p", "auto", "we did the thing "+sec+" end")
			if err := memCompactCapture(strings.NewReader(payload), spool, time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(spool, "raw-1-2.json"), []byte(payload), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := drainCompactions(context.Background(), db, spool); err != nil {
				t.Fatal(err)
			}
			if n := countRows(t, db, `SELECT count(*) FROM summaries`); n != 1 {
				t.Fatalf("summaries = %d, want 1", n)
			}
			var learned string
			db.QueryRow(`SELECT learned FROM summaries`).Scan(&learned)
			if learned != memdRedact(learned) {
				t.Fatalf("stored text is not a fixed point of redaction: %q", learned)
			}
		})
	}
}

// The project is the one projectKey gives (a repo subdirectory maps to the repo, a linked
// worktree to "<repo>/<worktree>"), not the cwd's basename.
func TestCompactionProjectFromGitCwd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	base := t.TempDir()
	repo := filepath.Join(base, "zorgrepo")
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(filepath.Join(repo, "sub", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	run("init", "-q")
	run("-c", "user.email=a@b", "-c", "user.name=a", "commit", "-q", "--allow-empty", "-m", "x")
	wt := filepath.Join(base, "wtfeature")
	run("worktree", "add", "-q", wt)
	for cwd, want := range map[string]string{filepath.Join(repo, "sub", "deep"): "zorgrepo", wt: "zorgrepo/wtfeature"} {
		db, _ := openTestMemd(t)
		rec := &compactRecord{SessionID: "s-proj", Trigger: "manual", Cwd: cwd, Summary: "proj text"}
		if _, err := storeCompaction(db, rec, time.Now()); err != nil {
			t.Fatal(err)
		}
		var got string
		db.QueryRow(`SELECT project FROM summaries`).Scan(&got)
		if got != want {
			t.Errorf("cwd %s: project %q, want %q", cwd, got, want)
		}
	}
}

// With no cwd the project is the session's known project, else unknown-project.
func TestCompactionEmptyCwdUsesSessionsThenUnknown(t *testing.T) {
	db, _ := openTestMemd(t)
	if _, err := db.Exec(`INSERT INTO sessions (session_id, project) VALUES ('s-known', 'knownproj')`); err != nil {
		t.Fatal(err)
	}
	for sid, want := range map[string]string{"s-known": "knownproj", "s-unk": memdUnknownProject} {
		rec := &compactRecord{SessionID: sid, Trigger: "auto", Summary: "text for " + sid}
		if _, err := storeCompaction(db, rec, time.Now()); err != nil {
			t.Fatal(err)
		}
		var p string
		if err := db.QueryRow(`SELECT project FROM summaries WHERE session_id = ?`, sid).Scan(&p); err != nil || p != want {
			t.Errorf("%s: project %q (err %v), want %q", sid, p, err, want)
		}
	}
}

// The stored text is cut at compactSummaryMax.
func TestCompactionSummaryClipped(t *testing.T) {
	db, _ := openTestMemd(t)
	rec := &compactRecord{SessionID: "s-clip", Trigger: "auto", Cwd: "/w/clip", Summary: strings.Repeat("abcdefgh ", 40000)}
	if _, err := storeCompaction(db, rec, time.Now()); err != nil {
		t.Fatal(err)
	}
	var learned string
	db.QueryRow(`SELECT learned FROM summaries`).Scan(&learned)
	if len(learned) > compactSummaryMax+64 || !strings.HasSuffix(learned, "[summary cut]") {
		t.Fatalf("stored %d bytes, want at most %d plus the marker", len(learned), compactSummaryMax)
	}
}

// A spool file that fails validation (unsafe or missing session id, no summary) is set aside,
// never stored.
func TestDrainSetsAsideInvalidRecords(t *testing.T) {
	db, _ := openTestMemd(t)
	spool := filepath.Join(t.TempDir(), "compactions")
	if err := os.MkdirAll(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"a.json": compactPayload("../evil", "/w", "auto", "text"),
		"b.json": compactPayload("", "/w", "auto", "text"),
		"c.json": compactPayload("ok-id", "/w", "auto", "   "),
	} {
		if err := os.WriteFile(filepath.Join(spool, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := drainCompactions(context.Background(), db, spool); err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := countRows(t, db, `SELECT count(*) FROM summaries`); got != 0 {
		t.Fatalf("invalid records were stored: %d rows", got)
	}
	if rej, _ := filepath.Glob(filepath.Join(spool, "rejected", "*.json")); len(rej) != 3 {
		t.Fatalf("set aside %v, want 3", rej)
	}
}

// Hook writers and the drain run at once: no torn reads (nothing rejected), nothing lost, no
// duplicates. This is what the tmp-then-rename write protects.
func TestConcurrentCapturesAndDrain(t *testing.T) {
	db, _ := openTestMemd(t)
	spool := filepath.Join(t.TempDir(), "compactions")
	const writers, each = 4, 12
	stop := make(chan struct{})
	var drainErr error
	var dwg sync.WaitGroup
	dwg.Add(1)
	go func() {
		defer dwg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := drainCompactions(context.Background(), db, spool); err != nil {
				drainErr = err
			}
		}
	}()
	body := strings.Repeat("lorem ipsum dolor ", 20000) // large enough to be caught mid-write
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				sid := fmt.Sprintf("w%d-%d", w, i)
				if err := memCompactCapture(strings.NewReader(compactPayload(sid, "/n/p", "auto", body+sid)), spool, time.Now()); err != nil {
					t.Error(err)
				}
			}
		}(w)
	}
	wg.Wait()
	close(stop)
	dwg.Wait()
	if _, err := drainCompactions(context.Background(), db, spool); err != nil {
		t.Fatal(err)
	}
	if drainErr != nil {
		t.Errorf("drain error during the race: %v", drainErr)
	}
	if n := countRows(t, db, `SELECT count(*) FROM summaries`); n != writers*each {
		t.Errorf("summaries = %d, want %d", n, writers*each)
	}
	if rej, _ := filepath.Glob(filepath.Join(spool, "rejected", "*")); len(rej) != 0 {
		t.Errorf("%d records rejected during the race (torn reads)", len(rej))
	}
}

// get_observations: numeric ids are unchanged by S-id support, mixed ids return both, and the
// project filter applies to both kinds.
func TestGetObservationsMixedIDsAndProject(t *testing.T) {
	db, _ := openTestMemd(t)
	if _, err := db.Exec(`INSERT INTO observations (project, session_id, type, title, narrative, source) VALUES ('pa','s','bugfix','obs title alpha','narr alpha','live')`); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, c := range []struct{ sid, proj, text string }{{"s", "pa", "compaction text gamma"}, {"s2", "pb", "compaction text delta"}} {
		if _, err := db.Exec(`INSERT INTO summaries (project, session_id, request, learned, source, created_at) VALUES (?, ?, 'Context compaction (auto)', ?, 'live', CAST(? AS TIMESTAMP))`,
			c.proj, c.sid, c.text, memdTS(now)); err != nil {
			t.Fatal(err)
		}
	}
	num, err := memGetObservations(db, map[string]any{"ids": []any{float64(1)}})
	if err != nil || !strings.Contains(num, "obs title alpha") || strings.Contains(num, "Session summaries") {
		t.Fatalf("numeric: %v\n%s", err, num)
	}
	mixed, err := memGetObservations(db, map[string]any{"ids": []any{float64(1), "S1", "#S2"}})
	if err != nil || !strings.Contains(mixed, "obs title alpha") || !strings.Contains(mixed, "gamma") || !strings.Contains(mixed, "delta") {
		t.Fatalf("mixed: %v\n%s", err, mixed)
	}
	pf, _ := memGetObservations(db, map[string]any{"ids": []any{float64(1), "S1", "S2"}, "project": "pb"})
	if strings.Contains(pf, "alpha") || strings.Contains(pf, "gamma") || !strings.Contains(pf, "delta") {
		t.Errorf("project filter:\n%s", pf)
	}
	onlyS, _ := memGetObservations(db, map[string]any{"ids": []any{"S1", "S2"}, "project": "pb"})
	if strings.Contains(onlyS, "gamma") || !strings.Contains(onlyS, "delta") {
		t.Errorf("project filter on S ids only:\n%s", onlyS)
	}
	if none, _ := memGetObservations(db, map[string]any{"ids": []any{"S99"}}); !strings.Contains(none, "None of those IDs") {
		t.Errorf("S99: %s", none)
	}
	for _, bad := range []string{"S0", "Sx"} {
		if _, err := memGetObservations(db, map[string]any{"ids": []any{bad}}); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
