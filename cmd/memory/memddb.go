package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/duckdb/duckdb-go/v2"
)

// memdDir is the state dir: $MEMORY_DIR, else $XDG_DATA_HOME/memory (default
// ~/.local/share/memory). A relative XDG_DATA_HOME is ignored, per the XDG rule.
func memdDir() string {
	if d := os.Getenv("MEMORY_DIR"); d != "" {
		return d
	}
	if x := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "memory")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "memory")
}

// memdExtDir holds the signed DuckDB extensions: $MEMORY_EXT_DIR, default <state dir>/ext.
func memdExtDir() string {
	if d := os.Getenv("MEMORY_EXT_DIR"); d != "" {
		return d
	}
	return filepath.Join(memdDir(), "ext")
}

// sqlQuote renders s as a single-quoted SQL string literal.
func sqlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// openMemdDB opens dir/memory.duckdb, loads the fts extension from a local signed file (no
// install, no autoload, no download) and runs memdMigrate. Only memory opens this file.
func openMemdDB(dir string) (*sql.DB, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("memory dir: %w", err)
	}
	ext := filepath.Join(memdExtDir(), "fts.duckdb_extension")
	if _, err := os.Stat(ext); err != nil {
		return nil, fmt.Errorf("fts extension missing at %s (set MEMORY_EXT_DIR): %w", ext, err)
	}
	db, err := sql.Open("duckdb", filepath.Join(dir, "memory.duckdb"))
	if err != nil {
		return nil, fmt.Errorf("open memory.duckdb: %w", err)
	}
	// These settings and the LOAD are database-global, so every pooled connection has them
	// (pinned by TestMemdSettingsPerConnection). TimeZone is UTC so DEFAULT current_timestamp
	// stores UTC like memdTS does (TestMemdDefaultTimestampsAreUTC).
	for _, s := range []string{
		"SET GLOBAL TimeZone='UTC'",
		"SET autoinstall_known_extensions=false",
		"SET autoload_known_extensions=false",
		"LOAD " + sqlQuote(ext),
	} {
		if _, err := db.Exec(s); err != nil {
			db.Close()
			return nil, fmt.Errorf("memory setup %q: %w", s, err)
		}
	}
	if err := memdMigrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Ids come from sequences so live inserts need no id; explicit ids are
// allowed and the sequence is bumped afterwards. facts and concepts are JSON arrays
// stored as VARCHAR so the fts index can read them. Text columns are NOT NULL DEFAULT ”
// to keep NULLs out of the fts index.
const memdSchema = `
CREATE SEQUENCE IF NOT EXISTS observations_seq;
CREATE SEQUENCE IF NOT EXISTS summaries_seq;
CREATE SEQUENCE IF NOT EXISTS queue_seq;
CREATE SEQUENCE IF NOT EXISTS prompts_seq;
CREATE SEQUENCE IF NOT EXISTS tool_uses_seq;
CREATE SEQUENCE IF NOT EXISTS work_state_seq;
CREATE SEQUENCE IF NOT EXISTS deliverables_seq;
CREATE SEQUENCE IF NOT EXISTS loss_receipts_seq;

CREATE TABLE IF NOT EXISTS transcript_offsets (
	path        VARCHAR PRIMARY KEY,
	session_id  VARCHAR NOT NULL DEFAULT '',
	byte_offset BIGINT NOT NULL DEFAULT 0,
	inode       BIGINT NOT NULL DEFAULT 0,
	size        BIGINT NOT NULL DEFAULT 0,
	observer    BOOLEAN NOT NULL DEFAULT false,
	sub         BOOLEAN NOT NULL DEFAULT false,
	updated_at  TIMESTAMP NOT NULL DEFAULT current_timestamp
);
CREATE TABLE IF NOT EXISTS sessions (
	session_id   VARCHAR PRIMARY KEY,
	project      VARCHAR NOT NULL DEFAULT '',
	cwd          VARCHAR NOT NULL DEFAULT '',
	is_observer  BOOLEAN NOT NULL DEFAULT false,
	started_at   TIMESTAMP NOT NULL DEFAULT current_timestamp,
	last_seen_at TIMESTAMP NOT NULL DEFAULT current_timestamp
);
CREATE TABLE IF NOT EXISTS prompts (
	id            BIGINT PRIMARY KEY DEFAULT nextval('prompts_seq'),
	session_id    VARCHAR NOT NULL,
	prompt_number INTEGER NOT NULL DEFAULT 0,
	text          VARCHAR NOT NULL DEFAULT '',
	created_at    TIMESTAMP NOT NULL DEFAULT current_timestamp,
	UNIQUE (session_id, created_at)
);
CREATE TABLE IF NOT EXISTS tool_uses (
	id          BIGINT PRIMARY KEY DEFAULT nextval('tool_uses_seq'),
	session_id  VARCHAR NOT NULL,
	tool_use_id VARCHAR NOT NULL,
	tool_name   VARCHAR NOT NULL DEFAULT '',
	input       VARCHAR NOT NULL DEFAULT '',
	output      VARCHAR NOT NULL DEFAULT '',
	created_at  TIMESTAMP NOT NULL DEFAULT current_timestamp,
	UNIQUE (session_id, tool_use_id)
);
CREATE TABLE IF NOT EXISTS queue (
	id                    BIGINT PRIMARY KEY DEFAULT nextval('queue_seq'),
	session_id            VARCHAR NOT NULL,
	tool_use_id           VARCHAR NOT NULL DEFAULT '',
	state                 VARCHAR NOT NULL DEFAULT 'queued' CHECK (state IN ('queued','claimed','done','rejected','unknown')),
	lease_until           TIMESTAMP,
	attempts              INTEGER NOT NULL DEFAULT 0,
	reason                VARCHAR NOT NULL DEFAULT '',
	dedupe_key            VARCHAR NOT NULL DEFAULT '',
	input_tokens          BIGINT NOT NULL DEFAULT 0,
	output_tokens         BIGINT NOT NULL DEFAULT 0,
	cache_read_tokens     BIGINT NOT NULL DEFAULT 0,
	cache_creation_tokens BIGINT NOT NULL DEFAULT 0,
	cost_usd              DOUBLE NOT NULL DEFAULT 0,
	created_at            TIMESTAMP NOT NULL DEFAULT current_timestamp,
	updated_at            TIMESTAMP NOT NULL DEFAULT current_timestamp
);
CREATE TABLE IF NOT EXISTS observations (
	id                 BIGINT PRIMARY KEY DEFAULT nextval('observations_seq'),
	project            VARCHAR NOT NULL DEFAULT '',
	session_id         VARCHAR NOT NULL DEFAULT '',
	type               VARCHAR NOT NULL DEFAULT '',
	raw_type           VARCHAR NOT NULL DEFAULT '',
	title              VARCHAR NOT NULL DEFAULT '',
	subtitle           VARCHAR NOT NULL DEFAULT '',
	narrative          VARCHAR NOT NULL DEFAULT '',
	facts              VARCHAR NOT NULL DEFAULT '[]',
	concepts           VARCHAR NOT NULL DEFAULT '[]',
	prompt_version     VARCHAR NOT NULL DEFAULT '',
	generated_by_model VARCHAR NOT NULL DEFAULT '',
	dedupe_key         VARCHAR NOT NULL DEFAULT '',
	source             VARCHAR NOT NULL CHECK (source IN ('imported','live')),
	created_at         TIMESTAMP NOT NULL DEFAULT current_timestamp
);
CREATE TABLE IF NOT EXISTS observation_files (
	observation_id BIGINT NOT NULL,
	path           VARCHAR NOT NULL,
	kind           VARCHAR NOT NULL CHECK (kind IN ('read','modified'))
);
CREATE TABLE IF NOT EXISTS summaries (
	id                 BIGINT PRIMARY KEY DEFAULT nextval('summaries_seq'),
	project            VARCHAR NOT NULL DEFAULT '',
	session_id         VARCHAR NOT NULL DEFAULT '',
	request            VARCHAR NOT NULL DEFAULT '',
	investigated       VARCHAR NOT NULL DEFAULT '',
	learned            VARCHAR NOT NULL DEFAULT '',
	completed          VARCHAR NOT NULL DEFAULT '',
	next_steps         VARCHAR NOT NULL DEFAULT '',
	notes              VARCHAR NOT NULL DEFAULT '',
	prompt_version     VARCHAR NOT NULL DEFAULT '',
	generated_by_model VARCHAR NOT NULL DEFAULT '',
	source             VARCHAR NOT NULL CHECK (source IN ('imported','live')),
	created_at         TIMESTAMP NOT NULL DEFAULT current_timestamp
);
-- One row per captured compaction (PostCompact hook): the idempotency key and the marker that
-- summary_id's summaries row is a compaction summary, not a model-written session summary.
CREATE TABLE IF NOT EXISTS compactions (
	capture_key VARCHAR PRIMARY KEY,
	summary_id  BIGINT NOT NULL,
	session_id  VARCHAR NOT NULL DEFAULT '',
	trigger     VARCHAR NOT NULL DEFAULT '',
	cwd         VARCHAR NOT NULL DEFAULT '',
	created_at  TIMESTAMP NOT NULL DEFAULT current_timestamp
);
CREATE TABLE IF NOT EXISTS summary_requests (
	session_id VARCHAR PRIMARY KEY,
	state      VARCHAR NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','done','rejected')),
	due_at     TIMESTAMP NOT NULL,
	attempts   INTEGER NOT NULL DEFAULT 0,
	reason     VARCHAR NOT NULL DEFAULT '',
	gen        INTEGER NOT NULL DEFAULT 0,
	updated_at TIMESTAMP NOT NULL DEFAULT current_timestamp
);
CREATE TABLE IF NOT EXISTS work_state (
	id         BIGINT PRIMARY KEY DEFAULT nextval('work_state_seq'),
	project    VARCHAR NOT NULL DEFAULT '',
	list_name  VARCHAR NOT NULL,
	fields     VARCHAR NOT NULL DEFAULT '{}',
	created_at TIMESTAMP NOT NULL DEFAULT current_timestamp
);
CREATE TABLE IF NOT EXISTS deliverables (
	id         BIGINT PRIMARY KEY DEFAULT nextval('deliverables_seq'),
	project    VARCHAR NOT NULL DEFAULT '',
	session_id VARCHAR NOT NULL DEFAULT '',
	path       VARCHAR NOT NULL DEFAULT '',
	copy_path  VARCHAR NOT NULL DEFAULT '',
	created_at TIMESTAMP NOT NULL DEFAULT current_timestamp
);
CREATE TABLE IF NOT EXISTS loss_receipts (
	id         BIGINT PRIMARY KEY DEFAULT nextval('loss_receipts_seq'),
	kind       VARCHAR NOT NULL DEFAULT '',
	session_id VARCHAR NOT NULL DEFAULT '',
	detail     VARCHAR NOT NULL DEFAULT '',
	n          INTEGER NOT NULL DEFAULT 1,
	created_at TIMESTAMP NOT NULL DEFAULT current_timestamp
);
CREATE TABLE IF NOT EXISTS observer_health (
	id           INTEGER PRIMARY KEY CHECK (id = 1),
	last_outcome VARCHAR NOT NULL DEFAULT '',
	last_reason  VARCHAR NOT NULL DEFAULT '',
	since        TIMESTAMP NOT NULL DEFAULT current_timestamp
);
CREATE TABLE IF NOT EXISTS import_watermarks (
	table_name VARCHAR PRIMARY KEY,
	last_id    BIGINT NOT NULL DEFAULT 0
);
-- One row per API response (message.id + requestId): a response is written to the transcript
-- once per content block, so the same key repeats. The cache_creation 5m/1h split is NULL
-- when the transcript has no cache_creation object. reset_kind is 'clear' or 'compact' when
-- the response is the first one after that boundary in the transcript.
CREATE TABLE IF NOT EXISTS api_usage (
	message_id                     VARCHAR NOT NULL,
	request_id                     VARCHAR NOT NULL DEFAULT '',
	session_id                     VARCHAR NOT NULL DEFAULT '',
	project                        VARCHAR NOT NULL DEFAULT '',
	cwd                            VARCHAR NOT NULL DEFAULT '',
	ts                             TIMESTAMP NOT NULL,
	model                          VARCHAR NOT NULL DEFAULT '',
	is_sidechain                   BOOLEAN NOT NULL DEFAULT false,
	is_observer                    BOOLEAN NOT NULL DEFAULT false,
	agent_id                       VARCHAR NOT NULL DEFAULT '',
	input_tokens                   BIGINT NOT NULL DEFAULT 0,
	output_tokens                  BIGINT NOT NULL DEFAULT 0,
	cache_creation_input_tokens    BIGINT NOT NULL DEFAULT 0,
	cache_creation_5m_input_tokens BIGINT,
	cache_creation_1h_input_tokens BIGINT,
	cache_read_input_tokens        BIGINT NOT NULL DEFAULT 0,
	service_tier                   VARCHAR NOT NULL DEFAULT '',
	reset_kind                     VARCHAR NOT NULL DEFAULT '',
	PRIMARY KEY (message_id, request_id)
);
`

// Index options verified in the Task 2 spike: incremental maintenance, no stemming or
// stopwords, and an ignore regex that keeps digits, dots and underscores so paths,
// identifiers and error codes stay searchable (the default regex strips them).
const ftsOpts = `stemmer='none', stopwords='none', ignore='[^a-z0-9._]+', incremental=1`

// memdMigrate creates the schema and the fts indexes; safe to call repeatedly. An existing
// index is left alone (rebuilding it with overwrite=1 would rescan every row on each start).
//
// observations and summaries are append-only: never UPDATE an fts-indexed column, because
// fts on DuckDB 2.0.0 maintains the index on INSERT and DELETE but not on UPDATE. Change
// such a row with DELETE + INSERT in one transaction.
func memdMigrate(db *sql.DB) error {
	if _, err := db.Exec(memdSchema); err != nil {
		return fmt.Errorf("memory schema: %w", err)
	}
	// Additive columns for stale-note detection; NULL on rows written before they existed.
	for _, c := range []string{"fingerprint", "fp_path"} {
		if _, err := db.Exec(`ALTER TABLE observation_files ADD COLUMN IF NOT EXISTS ` + c + ` VARCHAR`); err != nil {
			return fmt.Errorf("memory schema: add observation_files.%s: %w", c, err)
		}
	}
	// A /clear or compaction marker seen but not yet taken by a response, kept across restarts.
	if _, err := db.Exec(`ALTER TABLE transcript_offsets ADD COLUMN IF NOT EXISTS reset_kind VARCHAR DEFAULT ''`); err != nil {
		return fmt.Errorf("memory schema: add transcript_offsets.reset_kind: %w", err)
	}
	if _, err := db.Exec(memdSearchSchema); err != nil {
		return fmt.Errorf("memory search schema: %w", err)
	}
	indexes := []struct{ table, cols string }{
		{"observations", "'title','subtitle','narrative','facts','concepts'"},
		{"summaries", "'request','investigated','learned','completed','next_steps','notes'"},
	}
	for _, ix := range indexes {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM duckdb_schemas() WHERE schema_name = ?`, "fts_main_"+ix.table).Scan(&n); err != nil {
			return fmt.Errorf("check fts index %s: %w", ix.table, err)
		}
		if n > 0 {
			continue
		}
		pragma := fmt.Sprintf("PRAGMA create_fts_index('%s','id',%s, %s)", ix.table, ix.cols, ftsOpts)
		if _, err := db.Exec(pragma); err != nil {
			return fmt.Errorf("create fts index %s: %w", ix.table, err)
		}
	}
	// Postings for the keyword search; bulk-built the first time on an existing database.
	s := searcherFor(db)
	for _, k := range searchKinds {
		if err := s.backfill(context.Background(), k); err != nil {
			return fmt.Errorf("backfill search index %s: %w", k.table, err)
		}
	}
	return nil
}

// ftsSearch returns observation ids matching q, best score first. Minimal helper for
// tests; the real search lives with the query tools.
func ftsSearch(db *sql.DB, q string) ([]int64, error) {
	rows, err := db.Query(`SELECT id FROM (SELECT id, fts_main_observations.match_bm25(id, ?) AS s FROM observations)
		WHERE s IS NOT NULL ORDER BY s DESC, id`, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
