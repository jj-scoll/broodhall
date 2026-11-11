package main

// Fork grouping and subagent parents. `claude --resume X --fork-session` writes a new
// transcript under a new sessionId. Verified against claude 2.1.322 (print mode): the file
// has no forkedFrom field. It repeats the original's lines, message uuids included, so the
// first uuid in the file is the same in the original and in every fork of it. The in-session
// branch command does write `forkedFrom: {sessionId, messageUuid}` on each copied line, and
// that is stored as given.

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// memdMigrateFork adds the fork columns to sessions. forked_from is the session this one was
// forked from (” when none); root_uuid is the first message uuid of the transcript and
// opened_at the first timestamp in it (the fork time, since copied lines keep the original's
// times only after the queue-operation line that opens the file).
func memdMigrateFork(db *sql.DB) error {
	for _, c := range []string{
		`forked_from VARCHAR DEFAULT ''`,
		`root_uuid VARCHAR DEFAULT ''`,
		`opened_at TIMESTAMP`,
	} {
		if _, err := db.Exec(`ALTER TABLE sessions ADD COLUMN IF NOT EXISTS ` + c); err != nil {
			return fmt.Errorf("memory schema: add sessions.%s: %w", strings.Fields(c)[0], err)
		}
	}
	return nil
}

// transcriptSessionID is the session id a transcript path stands for when its lines name
// none: the file name, or for a subagent transcript (<session>/subagents/agent-<id>.jsonl)
// the parent session's directory.
func transcriptSessionID(path string) string {
	if dir := filepath.Dir(path); filepath.Base(dir) == "subagents" {
		sessionDir := filepath.Dir(dir)
		if filepath.Base(filepath.Dir(sessionDir)) != "projects" { // <slug>/<session>/subagents
			return filepath.Base(sessionDir)
		}
	}
	name := filepath.Base(path)
	return strings.TrimSuffix(name, filepath.Ext(name))
}

// linkFork records the fork facts of a main-thread transcript on its session row, then
// links the family: every session whose transcript starts with the same message uuid is a
// copy of the earliest-opened one, so the others are forks of it. An explicit forkedFrom is
// never overwritten. It does nothing when the session row does not exist yet (the next
// batch retries) or for subagent and observer files.
func linkFork(tx *sql.Tx, sessionID string, st *memdFileState) error {
	if sessionID == "" || st.Sub || st.Observer || (st.RootUUID == "" && st.ForkedFrom == "") {
		return nil
	}
	if st.ForkedFrom == sessionID {
		st.ForkedFrom = ""
	}
	opened := any(nil)
	if !st.OpenedAt.IsZero() {
		opened = memdTS(st.OpenedAt)
	}
	if _, err := tx.Exec(`UPDATE sessions SET
  root_uuid = CASE WHEN root_uuid = '' OR root_uuid IS NULL THEN ? ELSE root_uuid END,
  opened_at = coalesce(opened_at, CAST(? AS TIMESTAMP)),
  forked_from = CASE WHEN ? <> '' THEN ? ELSE forked_from END
WHERE session_id = ?`, st.RootUUID, opened, st.ForkedFrom, st.ForkedFrom, sessionID); err != nil {
		return fmt.Errorf("record fork facts: %w", err)
	}
	if st.RootUUID == "" {
		return nil
	}
	var orig string
	err := tx.QueryRow(`SELECT session_id FROM sessions WHERE root_uuid = ?
ORDER BY opened_at NULLS LAST, session_id LIMIT 1`, st.RootUUID).Scan(&orig)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // the session has no row yet (its events were all dropped): the next batch retries
	}
	if err != nil {
		return fmt.Errorf("find fork family: %w", err)
	}
	if _, err := tx.Exec(`UPDATE sessions SET forked_from = ?
WHERE root_uuid = ? AND session_id <> ? AND (forked_from = '' OR forked_from IS NULL)`, orig, st.RootUUID, orig); err != nil {
		return fmt.Errorf("link forks: %w", err)
	}
	return nil
}

// noteForkFacts latches the first message uuid, the first timestamp and any forkedFrom
// session id seen in a transcript line.
func noteForkFacts(d *obj, st *memdFileState) {
	if st.forkKnown {
		return
	}
	if st.RootUUID == "" {
		st.RootUUID = d.getStr("uuid")
	}
	if st.OpenedAt.IsZero() {
		if ts := d.getStr("timestamp"); ts != "" {
			st.OpenedAt, _ = time.Parse(time.RFC3339Nano, ts)
		}
	}
	if st.ForkedFrom == "" {
		if ff := d.getObj("forkedFrom"); ff != nil {
			st.ForkedFrom = ff.getStr("sessionId")
		}
	}
}
