package main

// Compaction capture. Claude Code's PostCompact hook hands over the summary the compaction
// produced; for a manual /compact the transcript does not carry it, so a hook is the only
// source. `memory compact-capture` (the hook) never opens memory.duckdb, which the daemon
// holds: it writes one redacted JSON record into <state dir>/compactions/ with a temp file and
// a rename, and exits. runCompactionDrain (in the daemon) ingests that directory. A spool
// outlives a stopped or missing daemon, so nothing is lost while it is down.
//
// A captured summary is a row in summaries (so search, timeline and get already reach it), with
// a side row in compactions that is the idempotency key and the marker of its source. The
// summaries table's source CHECK admits only 'imported' and 'live' and DuckDB cannot alter a
// CHECK, so the marker is the side table, not a new source value.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// compactSummaryMax caps the stored text in bytes. Real compaction summaries run a few KB to
// tens of KB.
const compactSummaryMax = 200 * 1024

// compactDrainPoll is how often the daemon looks at the spool. A var so tests need no sleeps.
var compactDrainPoll = 5 * time.Second

// compactTmpMaxAge is how long a .tmp spool file may sit before the drain deletes it: a hook
// writes and renames within milliseconds, so an older one is a killed writer's leftover.
// compactRejectedMaxAge is how long a set-aside capture is kept for inspection.
const (
	compactTmpMaxAge      = time.Hour
	compactRejectedMaxAge = 7 * 24 * time.Hour
)

var safeSessionID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// compactRecord is a spool file: the PostCompact payload fields memory uses. ts is absent from
// a raw payload that run.sh dropped when there was no binary; the file's mtime stands in.
type compactRecord struct {
	SessionID      string `json:"session_id"`
	TS             string `json:"ts,omitempty"`
	Trigger        string `json:"trigger"`
	Cwd            string `json:"cwd"`
	ProjectDir     string `json:"project_dir,omitempty"`
	TranscriptPath string `json:"transcript_path"`
	Summary        string `json:"compact_summary"`
}

func (r *compactRecord) validate() error {
	if !safeSessionID.MatchString(r.SessionID) {
		return fmt.Errorf("unsafe or missing session_id %q", r.SessionID)
	}
	if strings.TrimSpace(r.Summary) == "" {
		return errors.New("no compact_summary")
	}
	return nil
}

// memCompactCapture reads a PostCompact payload from in and writes the spool record. It masks
// secrets before anything touches disk.
func memCompactCapture(in io.Reader, spool string, now time.Time) error {
	data, err := io.ReadAll(io.LimitReader(in, 64<<20))
	if err != nil {
		return err
	}
	var r compactRecord
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("hook input: %w", err)
	}
	if err := r.validate(); err != nil {
		return err
	}
	// CLAUDE_PROJECT_DIR is what the other hooks name the project from; the cwd is the fallback.
	r.ProjectDir = os.Getenv("CLAUDE_PROJECT_DIR")
	r.Summary = memdRedact(r.Summary)
	r.TS = now.UTC().Format(time.RFC3339Nano)
	out, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(spool, 0o700); err != nil {
		return err
	}
	name := fmt.Sprintf("%s-%d.json", r.SessionID, now.UnixNano())
	tmp := filepath.Join(spool, "."+name+".tmp")
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(spool, name)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// memCompactCaptureCmd is `memory compact-capture`: it fails open (always 0) and prints
// nothing on stdout; a problem goes to stderr, where Claude Code shows it to the user only.
func memCompactCaptureCmd(in io.Reader, stderr io.Writer) int {
	if err := memCompactCapture(in, filepath.Join(memdDir(), "compactions"), time.Now()); err != nil {
		fmt.Fprintln(stderr, "memory compact-capture:", err)
	}
	return 0
}

// compactKey identifies one compaction: the session and the redacted summary text, so the
// same capture arriving twice (hook retry, or a crash after commit and before the file was
// removed) is one row, and a raw and a redacted copy of the same payload agree.
func compactKey(sessionID, summary string) string {
	h := sha256.Sum256([]byte(summary))
	return sessionID + ":" + hex.EncodeToString(h[:8])
}

// drainCompactions ingests every spool record and returns how many became new summaries. A
// file that cannot be a capture moves to rejected/ (kept, not retried); a database error stops
// the pass and leaves the file for the next one.
func drainCompactions(ctx context.Context, db *sql.DB, spool string) (int, error) {
	pruneSpool(spool, time.Now())
	// Only the top level: rejected/ is never read back.
	files, _ := filepath.Glob(filepath.Join(spool, "*.json"))
	sort.Strings(files)
	added := 0
	for _, f := range files {
		if ctx.Err() != nil {
			return added, nil
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var r compactRecord
		if err := json.Unmarshal(b, &r); err != nil || r.validate() != nil {
			setAside(spool, f)
			continue
		}
		when := time.Now()
		if t, err := time.Parse(time.RFC3339Nano, r.TS); err == nil {
			when = t
		} else if st, err := os.Stat(f); err == nil {
			when = st.ModTime()
		}
		isNew, err := storeCompaction(db, &r, when)
		if err != nil {
			return added, err
		}
		if isNew {
			added++
		}
		os.Remove(f)
	}
	return added, nil
}

// pruneSpool deletes stale .tmp files from the spool and set-aside captures past their keep time.
func pruneSpool(spool string, now time.Time) {
	remove := func(pattern string, maxAge time.Duration) {
		m, _ := filepath.Glob(pattern)
		for _, f := range m {
			if st, err := os.Stat(f); err == nil && st.Mode().IsRegular() && now.Sub(st.ModTime()) > maxAge {
				os.Remove(f)
			}
		}
	}
	remove(filepath.Join(spool, ".*.tmp"), compactTmpMaxAge)
	remove(filepath.Join(spool, "rejected", "*"), compactRejectedMaxAge)
}

func setAside(spool, f string) {
	rej := filepath.Join(spool, "rejected")
	dst := filepath.Join(rej, filepath.Base(f))
	if os.MkdirAll(rej, 0o700) == nil && os.Rename(f, dst) == nil {
		now := time.Now()
		os.Chtimes(dst, now, now) // the keep time starts now, not at the capture
		log.Printf("memory compact: set aside unusable capture %s", filepath.Base(f))
		return
	}
	log.Printf("memory compact: unusable capture %s (could not move it)", filepath.Base(f))
}

func storeCompaction(db *sql.DB, r *compactRecord, when time.Time) (bool, error) {
	summary := memdRedact(r.Summary) // a no-op on a hook-redacted record; covers a raw payload
	if len(summary) > compactSummaryMax {
		summary = clipBytes(summary, compactSummaryMax) + "\n[summary cut]"
	}
	key := compactKey(r.SessionID, memdRedact(r.Summary))
	trigger := r.Trigger
	if trigger != "manual" && trigger != "auto" {
		trigger = "unknown"
	}
	project := memdUnknownProject
	if dir := strings.TrimSpace(r.ProjectDir); dir != "" {
		project = projectKey(dir)
	} else if strings.TrimSpace(r.Cwd) != "" {
		project = projectKey(r.Cwd)
	} else {
		var p string
		if db.QueryRow(`SELECT project FROM sessions WHERE session_id = ?`, r.SessionID).Scan(&p) == nil && p != "" {
			project = p
		}
	}
	var sid int64
	isNew := false
	err := withWrite(func() error {
		isNew, sid = false, 0
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM compactions WHERE capture_key = ?`, key).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		err = tx.QueryRow(`INSERT INTO summaries (project, session_id, request, learned, source, created_at)
			VALUES (?, ?, ?, ?, 'live', CAST(? AS TIMESTAMP)) RETURNING id`,
			project, r.SessionID, "Context compaction ("+trigger+")", summary, memdTS(when)).Scan(&sid)
		if err != nil {
			return fmt.Errorf("insert compaction summary: %w", err)
		}
		if _, err := tx.Exec(`INSERT INTO compactions (capture_key, summary_id, session_id, trigger, cwd, created_at)
			VALUES (?, ?, ?, ?, ?, CAST(? AS TIMESTAMP))`, key, sid, r.SessionID, trigger, r.Cwd, memdTS(when)); err != nil {
			return fmt.Errorf("insert compaction: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		isNew = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if isNew {
		memdIndexSummary(db, sid)
	}
	return isNew, nil
}

// runCompactionDrain ingests the spool at start and then every compactDrainPoll until ctx ends.
func runCompactionDrain(ctx context.Context, db *sql.DB, spool string) {
	for {
		if n, err := drainCompactions(ctx, db, spool); err != nil {
			log.Printf("memory compact: %v", err)
		} else if n > 0 {
			log.Printf("memory compact: ingested %d compaction summaries", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(compactDrainPoll):
		}
	}
}
