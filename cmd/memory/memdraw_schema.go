package main

import (
	"database/sql"
	"fmt"
)

// Raw search: Claude's reply text, session branch and model, and the rx_* inverted index over
// the raw session records. See memdraw_index.go.
//
//	replies   one row per text block of an assistant message, masked; (session_id, reply_key)
//	          is unique, so re-reading a transcript adds nothing.
//	rx_docs   one row per indexed text: its session, kind (prompt, reply, tool, path), the
//	          source row id in prompts, replies or tool_uses, and the row's time.
//	rx_dict   term -> termid.
//	rx_post   (termid, doc, pos): token position within the doc, for phrase matching.
//	rx_state  per source table, the highest row id already indexed.
const memdRawSchema = `
CREATE SEQUENCE IF NOT EXISTS replies_seq;
CREATE TABLE IF NOT EXISTS replies (
	id         BIGINT PRIMARY KEY DEFAULT nextval('replies_seq'),
	session_id VARCHAR NOT NULL,
	reply_key  VARCHAR NOT NULL,
	text       VARCHAR NOT NULL DEFAULT '',
	created_at TIMESTAMP NOT NULL DEFAULT current_timestamp,
	UNIQUE (session_id, reply_key)
);
CREATE TABLE IF NOT EXISTS rx_docs (
	id         BIGINT NOT NULL,
	session_id VARCHAR NOT NULL,
	kind       VARCHAR NOT NULL,
	src_id     BIGINT NOT NULL,
	created_at TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS rx_dict (termid INTEGER NOT NULL, term VARCHAR NOT NULL);
CREATE TABLE IF NOT EXISTS rx_post (termid INTEGER NOT NULL, doc BIGINT NOT NULL, pos INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS rx_state (source VARCHAR PRIMARY KEY, last_id BIGINT NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS rx_dict_term ON rx_dict (term);
`

// memdRawMigrate creates the raw-search tables and adds the session columns. It does not
// index: rows the index has not seen are indexed in the background after start (see
// memdRawIndexBackground) or by the first search. Safe to call repeatedly.
func memdRawMigrate(db *sql.DB) error {
	if _, err := db.Exec(memdRawSchema); err != nil {
		return fmt.Errorf("memory raw schema: %w", err)
	}
	// The branch and model last seen in the session's transcript; empty on older rows.
	for _, c := range []string{"git_branch", "model"} {
		if _, err := db.Exec(`ALTER TABLE sessions ADD COLUMN IF NOT EXISTS ` + c + ` VARCHAR DEFAULT ''`); err != nil {
			return fmt.Errorf("memory schema: add sessions.%s: %w", c, err)
		}
	}
	return nil
}
