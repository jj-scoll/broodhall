package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	duckdb "github.com/duckdb/duckdb-go/v2"
)

// Raw search: free full-text search over the raw session records, with no model in the loop.
// The index (rx_*) covers prompts, Claude's replies, tool inputs (not outputs) and the file
// paths of Read, Edit, Write, MultiEdit and NotebookEdit calls. Semantics follow Atuin
// Threads: each word matches a whole word, "quoted words" match as a phrase in order, and
// every word or phrase of the query must occur somewhere in the session. An empty query lists
// the newest sessions. Results are sessions, newest first.
//
// Tokens: a run of letters, digits, '_' and '.', lower-cased, with leading and trailing dots
// dropped. A token holding '.' or '_' (parser.go, file_path) is also indexed as its parts at
// the same position, so "parser" finds parser.go and a phrase through it still lines up.
// Only the first rawMaxTokens tokens of each text are indexed, to bound the index.

const (
	rawMaxTokens   = 3000  // tokens indexed per text
	rawMaxTokenLen = 80    // longer runs (base64, minified blobs) are not words
	rawBatchRows   = 500   // source rows indexed per transaction
	rawMaxText     = 64000 // characters of one text read for indexing
	rawDefaultList = 20
)

type rawTok struct {
	term string
	pos  int
}

func isRawWordRune(r rune) bool {
	return r == '_' || r == '.' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// rawTokenize splits s into tokens. With stack, the parts of a compound token share its
// position. At most limit positions are produced.
func rawTokenize(s string, stack bool, limit int) []rawTok {
	var out []rawTok
	pos := 0
	emit := func(w string) bool {
		w = strings.Trim(w, ".")
		if w == "" || utf8.RuneCountInString(w) > rawMaxTokenLen {
			return true
		}
		out = append(out, rawTok{w, pos})
		if stack && strings.ContainsAny(w, "._") {
			for _, part := range strings.FieldsFunc(w, func(r rune) bool { return r == '.' || r == '_' }) {
				if part != w && utf8.RuneCountInString(part) >= 2 {
					out = append(out, rawTok{part, pos})
				}
			}
		}
		pos++
		return pos < limit
	}
	start := -1
	for i, r := range s {
		if isRawWordRune(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			if !emit(strings.ToLower(s[start:i])) {
				return out
			}
			start = -1
		}
	}
	if start >= 0 {
		emit(strings.ToLower(s[start:]))
	}
	return out
}

// rawUnit is one query term (a word) or a run of terms that must be adjacent and in order.
type rawUnit []string

// parseRawQuery splits a query into words and "quoted phrases". A bare word that breaks into
// several tokens (foo-bar) is a phrase. Duplicates are dropped.
func parseRawQuery(q string) []rawUnit {
	var units []rawUnit
	seen := map[string]bool{}
	add := func(text string) {
		toks := rawTokenize(text, false, 1<<20)
		if len(toks) == 0 {
			return
		}
		u := make(rawUnit, len(toks))
		for i, t := range toks {
			u[i] = t.term
		}
		if k := strings.Join(u, " "); !seen[k] {
			seen[k] = true
			units = append(units, u)
		}
	}
	rs := []rune(q)
	for i := 0; i < len(rs); {
		switch {
		case unicode.IsSpace(rs[i]):
			i++
		case rs[i] == '"':
			j := i + 1
			for j < len(rs) && rs[j] != '"' {
				j++
			}
			add(string(rs[i+1 : j]))
			i = j + 1
		default:
			j := i
			for j < len(rs) && !unicode.IsSpace(rs[j]) && rs[j] != '"' {
				j++
			}
			add(string(rs[i:j]))
			i = j
		}
	}
	return units
}

// ---- indexing ----

// rawSource describes one table the index reads. cols is the select list after id,
// session_id and created_at; docs turns a row into the texts to index.
type rawSource struct {
	name  string // rx_state key
	table string
	cols  string // select list, n values
	n     int
	docs  func(vals []string) []rawText
}

type rawText struct{ kind, text string }

var rawSources = []rawSource{
	{"prompts", "prompts", fmt.Sprintf("left(text, %d)", rawMaxText), 1, func(v []string) []rawText {
		return []rawText{{"prompt", v[0]}}
	}},
	{"replies", "replies", fmt.Sprintf("left(text, %d)", rawMaxText), 1, func(v []string) []rawText {
		return []rawText{{"reply", v[0]}}
	}},
	{"tool_uses", "tool_uses", fmt.Sprintf("tool_name, left(input, %d), input", rawMaxText), 3, func(v []string) []rawText {
		docs := []rawText{{"tool", v[1]}}
		if p := rawToolPath(v[0], v[2]); p != "" {
			docs = append(docs, rawText{"path", p})
		}
		return docs
	}},
}

// rawToolPath is the file path a Read, Edit, Write, MultiEdit or NotebookEdit input names.
func rawToolPath(tool, input string) string {
	switch tool {
	case "Read", "Edit", "Write", "MultiEdit", "NotebookEdit":
	default:
		return ""
	}
	var in map[string]any
	if json.Unmarshal([]byte(input), &in) != nil {
		return ""
	}
	for _, k := range []string{"file_path", "notebook_path"} {
		if p, _ := in[k].(string); p != "" {
			return p
		}
	}
	return ""
}

var rawIndexMu sync.Mutex

// memdRawIndex indexes every prompt, reply and tool call added since the last run, skipping
// observer sessions. The caller holds memdWriteMu or is the only writer (migration); a failure
// leaves rx_state where it was, so the next run retries the same rows.
func memdRawIndex(db *sql.DB) error {
	rawIndexMu.Lock()
	defer rawIndexMu.Unlock()
	t0 := time.Now()
	total := 0
	for _, src := range rawSources {
		for {
			n, err := rawIndexBatch(db, src)
			if err != nil {
				return fmt.Errorf("index %s: %w", src.table, err)
			}
			total += n
			if n < rawBatchRows {
				break
			}
		}
	}
	if total > rawBatchRows {
		log.Printf("memory raw search: indexed %d rows in %v", total, time.Since(t0).Round(time.Millisecond))
	}
	return nil
}

// rawLiveSteps bounds the index work done under one live write: a backlog (a database whose
// index the background has not finished) is left to the background and to searches.
const rawLiveSteps = 4

// rawStepHook, when set (tests), runs after each batch round of rawIndexAll.
var rawStepHook func()

// rawIndexStep indexes one batch of each source and reports whether any source has more. The
// caller holds memdWriteMu.
func rawIndexStep(db *sql.DB) (more bool, err error) {
	rawIndexMu.Lock()
	defer rawIndexMu.Unlock()
	for _, src := range rawSources {
		n, err := rawIndexBatch(db, src)
		if err != nil {
			return false, fmt.Errorf("index %s: %w", src.table, err)
		}
		more = more || n >= rawBatchRows
	}
	return more, nil
}

// memdRawIndexLogged is the write path's index call: a failure must not fail the capture, and
// the next search or write retries. It does at most rawLiveSteps batch rounds, so a backlog
// never holds the write lock for long. The caller holds memdWriteMu.
func memdRawIndexLogged(db *sql.DB) {
	for i := 0; i < rawLiveSteps; i++ {
		more, err := rawIndexStep(db)
		if err != nil {
			log.Printf("memory raw search: %v (the next search catches up)", err)
			return
		}
		if !more {
			return
		}
	}
}

// rawIndexAll indexes everything behind the watermarks, taking the write lock one batch round
// at a time so hooks and the tailer are served in between. It stops early when ctx ends.
func rawIndexAll(ctx context.Context, db *sql.DB) error {
	for {
		var more bool
		err := withWrite(func() (err error) { more, err = rawIndexStep(db); return })
		if err != nil {
			return err
		}
		if rawStepHook != nil {
			rawStepHook()
		}
		if !more {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// memdRawIndexBackground catches the index up off the start path: the daemon runs it after
// migrating, and searches wait for what is left (rawCatchUp).
func memdRawIndexBackground(ctx context.Context, db *sql.DB) error {
	behind, err := rawBehind(db)
	if err != nil || !behind {
		return err
	}
	t0 := time.Now()
	if err := rawIndexAll(ctx, db); err != nil {
		return err
	}
	log.Printf("memory raw search: background index caught up in %v", time.Since(t0).Round(time.Millisecond))
	return nil
}

// rawBehind reports whether any source table has rows past its watermark.
func rawBehind(db *sql.DB) (bool, error) {
	for _, src := range rawSources {
		var behind bool
		err := db.QueryRow(fmt.Sprintf(`SELECT coalesce((SELECT max(id) FROM %s), 0) > coalesce((SELECT last_id FROM rx_state WHERE source = ?), 0)`, src.table), src.name).Scan(&behind)
		if err != nil {
			return false, err
		}
		if behind {
			return true, nil
		}
	}
	return false, nil
}

// rawCatchUp indexes rows that landed behind the write hook (a direct insert, a failed index,
// a backlog the background has not reached). A search needs the whole index, so it waits for
// it, but batch by batch: the write lock is free between batches.
func rawCatchUp(db *sql.DB) error {
	behind, err := rawBehind(db)
	if err != nil || !behind {
		return err
	}
	return rawIndexAll(context.Background(), db)
}

type rawRow struct {
	id      int64
	session string
	created int64 // microseconds since the epoch, UTC
	vals    []string
}

// rawIndexBatch indexes up to rawBatchRows rows of src past its watermark in one transaction
// and returns how many rows it passed over.
func rawIndexBatch(db *sql.DB, src rawSource) (int, error) {
	// One connection for the transaction and the appenders, which bulk-write inside it.
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var last int64
	if err := tx.QueryRow(`SELECT coalesce((SELECT last_id FROM rx_state WHERE source = ?), 0)`, src.name).Scan(&last); err != nil {
		return 0, err
	}
	nvals := src.n
	rows, err := tx.Query(fmt.Sprintf(`WITH b AS (SELECT id, session_id, epoch_us(created_at) AS ts, %s FROM %s WHERE id > ? ORDER BY id LIMIT ?)
SELECT b.*, coalesce(s.is_observer, false) FROM b LEFT JOIN sessions s ON s.session_id = b.session_id ORDER BY b.id`, src.cols, src.table), last, rawBatchRows)
	if err != nil {
		return 0, err
	}
	var batch []rawRow
	var passed int
	for rows.Next() {
		r := rawRow{vals: make([]string, nvals)}
		dst := []any{&r.id, &r.session, &r.created}
		for i := range r.vals {
			dst = append(dst, &r.vals[i])
		}
		var observer bool
		dst = append(dst, &observer)
		if err := rows.Scan(dst...); err != nil {
			rows.Close()
			return 0, err
		}
		passed++
		last = r.id
		if !observer {
			batch = append(batch, r)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if passed == 0 {
		return 0, nil
	}

	var nextDoc int64
	var nextTerm int32
	if err := tx.QueryRow(`SELECT coalesce(max(id), 0) + 1 FROM rx_docs`).Scan(&nextDoc); err != nil {
		return 0, err
	}
	if err := tx.QueryRow(`SELECT coalesce(max(termid) + 1, 0) FROM rx_dict`).Scan(&nextTerm); err != nil {
		return 0, err
	}
	type doc struct {
		id      int64
		session string
		kind    string
		src     int64
		created time.Time
		toks    []rawTok
	}
	var docs []doc
	termSet := map[string]bool{}
	for _, r := range batch {
		for _, d := range src.docs(r.vals) {
			toks := rawTokenize(d.text, true, rawMaxTokens)
			if len(toks) == 0 {
				continue
			}
			for _, t := range toks {
				termSet[t.term] = true
			}
			docs = append(docs, doc{nextDoc, r.session, d.kind, r.id, time.UnixMicro(r.created).UTC(), toks})
			nextDoc++
		}
	}

	termID := make(map[string]int32, len(termSet))
	terms := make([]string, 0, len(termSet))
	for t := range termSet {
		terms = append(terms, t)
	}
	for lo := 0; lo < len(terms); lo += sqlInsertChunk {
		hi := min(lo+sqlInsertChunk, len(terms))
		args := make([]any, 0, hi-lo)
		for _, t := range terms[lo:hi] {
			args = append(args, t)
		}
		rs, err := tx.Query(`SELECT term, termid FROM rx_dict WHERE term IN (`+strings.TrimSuffix(strings.Repeat("?,", hi-lo), ",")+`)`, args...)
		if err != nil {
			return 0, err
		}
		for rs.Next() {
			var t string
			var id int32
			if err := rs.Scan(&t, &id); err != nil {
				rs.Close()
				return 0, err
			}
			termID[t] = id
		}
		rs.Close()
		if err := rs.Err(); err != nil {
			return 0, err
		}
	}
	var fresh []string
	for _, t := range terms {
		if _, ok := termID[t]; !ok {
			termID[t] = nextTerm
			nextTerm++
			fresh = append(fresh, t)
		}
	}
	err = rawAppend(conn, "rx_dict", func(app *duckdb.Appender) error {
		for _, t := range fresh {
			if err := app.AppendRow(termID[t], t); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("rx_dict: %w", err)
	}
	err = rawAppend(conn, "rx_docs", func(app *duckdb.Appender) error {
		for _, d := range docs {
			if err := app.AppendRow(d.id, d.session, d.kind, d.src, d.created); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("rx_docs: %w", err)
	}
	err = rawAppend(conn, "rx_post", func(app *duckdb.Appender) error {
		for _, d := range docs {
			for _, t := range d.toks {
				if err := app.AppendRow(termID[t.term], d.id, int32(t.pos)); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("rx_post: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO rx_state (source, last_id) VALUES (?, ?) ON CONFLICT (source) DO UPDATE SET last_id = excluded.last_id`, src.name, last); err != nil {
		return 0, err
	}
	return passed, tx.Commit()
}

// rawAppend bulk-writes table through an appender on conn, inside the transaction open on it.
func rawAppend(conn *sql.Conn, table string, fill func(app *duckdb.Appender) error) error {
	return conn.Raw(func(dc any) error {
		app, err := duckdb.NewAppenderFromConn(dc.(driver.Conn), "", table)
		if err != nil {
			return err
		}
		if err := fill(app); err != nil {
			app.Close()
			return err
		}
		return app.Close()
	})
}
