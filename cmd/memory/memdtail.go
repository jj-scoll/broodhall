package main

// memory tailer: follows Claude Code transcript JSONL files, feeds the lines to parseMemdLines
// and writes sessions, prompts, tool_uses and queue rows. Offsets live in transcript_offsets
// together with the file's inode, size and latched observer/sub flags, so a restart resumes
// where the last committed batch ended. Every write is idempotent, so a re-read from 0 (new
// inode, or a file that shrank) adds nothing twice.

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	memdPendingMaxAge = time.Hour
	memdPendingCap    = 1000
	memdScanEvery     = 5 * time.Second
	memdEventDebounce = 200 * time.Millisecond
	memdSkipMargin    = time.Minute
)

// memdTailFile is what the tailer remembers about one transcript between Scan calls.
type memdTailFile struct {
	st     memdFileState
	offset int64
	inode  int64
	size   int64
	known  bool // offset, inode, size and latches are set (from the DB or a skip)
	// resumed: the file had a stored offset at startup, so it is read on from there with no
	// startedAt filter and lines written while the daemon was down are kept.
	resumed bool
}

type memdTailer struct {
	db        *sql.DB
	roots     map[string]string // root dir -> account
	startedAt time.Time

	mu     sync.Mutex
	files  map[string]*memdTailFile
	loaded bool // stored offsets were read into files
}

// newMemdTailer returns a tailer over roots (root dir -> account). Files with a stored offset
// resume from it; in any other file, events stamped before startedAt are not recorded.
func newMemdTailer(db *sql.DB, roots map[string]string, startedAt time.Time) *memdTailer {
	return &memdTailer{db: db, roots: roots, startedAt: startedAt, files: map[string]*memdTailFile{}}
}

// Scan makes one pass over every *.jsonl under the roots and returns how many prompt and
// tool_use rows were newly inserted. A failing file does not stop the pass; the first error
// is returned after the rest were tried.
func (t *memdTailer) Scan() (newEvents int, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.loaded {
		if err := t.loadOffsets(); err != nil {
			return 0, err
		}
		t.loaded = true
	}
	for root, account := range t.roots {
		walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
			if werr != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return nil // vanished or unreadable entries are retried on the next pass
			}
			n, ferr := t.scanFile(path, account)
			newEvents += n
			if ferr != nil && err == nil {
				err = fmt.Errorf("tail %s: %w", path, ferr)
			}
			return nil
		})
		if walkErr != nil && err == nil {
			err = walkErr
		}
	}
	return newEvents, err
}

// loadOffsets reads every stored offset in one query, so the first pass over thousands of
// transcripts does not look each one up.
func (t *memdTailer) loadOffsets() error {
	rows, err := t.db.Query(`SELECT path, byte_offset, inode, size, observer, sub FROM transcript_offsets`)
	if err != nil {
		return fmt.Errorf("load offsets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		f := &memdTailFile{known: true, resumed: true}
		if err := rows.Scan(&path, &f.offset, &f.inode, &f.size, &f.st.Observer, &f.st.Sub); err != nil {
			return fmt.Errorf("load offsets: %w", err)
		}
		t.files[path] = f
	}
	return rows.Err()
}

func (t *memdTailer) scanFile(path, account string) (int, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, nil // removed between walk and stat
	}
	inode := int64(0)
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		inode = int64(sys.Ino)
	}
	size := fi.Size()

	f := t.files[path]
	if f == nil {
		f = &memdTailFile{}
		t.files[path] = f
	}
	// A file with no stored offset that was not written since startedAt holds no event to
	// record: start at its end without reading it. The latches are re-set by any later line.
	// The margin covers mtime's coarse clock; events before startedAt in such a file read
	// anyway are dropped.
	if !f.known {
		f.known = true
		if fi.ModTime().Before(t.startedAt.Add(-memdSkipMargin)) {
			f.offset, f.inode, f.size = size, inode, size
			return 0, nil
		}
	}
	// A new inode, or a file smaller than what was already consumed or seen, is a rewrite:
	// start over. The latches reset too; parseMemdLines re-applies the path rule.
	if f.offset > 0 && (inode != f.inode || size < f.offset || size < f.size) {
		f.st = memdFileState{}
		f.offset, f.size = 0, 0
	}
	if size == f.offset {
		f.inode, f.size = inode, size
		return 0, nil
	}

	fh, err := os.Open(path)
	if err != nil {
		return 0, nil
	}
	defer fh.Close()
	if _, err := fh.Seek(f.offset, io.SeekStart); err != nil {
		return 0, err
	}
	data, err := io.ReadAll(fh)
	if err != nil {
		return 0, err
	}
	end := strings.LastIndexByte(string(data), '\n') + 1 // only whole lines are consumed
	if end == 0 {
		return 0, nil
	}
	var lines [][]byte
	for _, l := range strings.Split(string(data[:end-1]), "\n") {
		if l != "" {
			lines = append(lines, []byte(l))
		}
	}

	// Parse and commit under memdWriteMu (R31: tailer, workers and summaries never write at
	// once). Every attempt parses from a fresh copy of the state, so a failed transaction (or
	// a conflict retried by withWrite) leaves the tailer able to re-read the same bytes with
	// the pending map as it was.
	saved := f.st
	saved.Pending = make(map[string]*memdEvent, len(f.st.Pending))
	for k, v := range f.st.Pending {
		c := *v
		saved.Pending[k] = &c
	}
	var n int
	err = withWrite(func() error {
		f.st = saved
		f.st.Pending = make(map[string]*memdEvent, len(saved.Pending))
		for k, v := range saved.Pending {
			c := *v
			f.st.Pending[k] = &c
		}
		events := parseMemdLines(path, account, lines, &f.st)
		var cerr error
		n, cerr = t.commit(path, f, events, inode, f.offset+int64(end), size)
		return cerr
	})
	if err != nil {
		f.st = saved
		return 0, err
	}
	f.offset += int64(end)
	f.inode, f.size = inode, size
	return n, nil
}

// commit writes one file's batch and its offset in a single transaction.
func (t *memdTailer) commit(path string, f *memdTailFile, events []memdEvent, inode, newOffset, size int64) (inserted int, err error) {
	tx, err := t.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	newest := time.Time{}
	var sessionID string
	var usage []memdUsage
	for _, e := range events {
		if e.At.After(newest) {
			newest = e.At
		}
		if e.SessionID != "" {
			sessionID = e.SessionID
		}
		if e.Kind == "usage" { // an idempotent upsert, so the startedAt filter does not apply
			usage = append(usage, *e.Usage)
			continue
		}
		if !f.resumed && !e.At.IsZero() && e.At.Before(t.startedAt) {
			continue
		}
		if e.Kind == "turn_end" {
			if err := requestSummary(tx, e); err != nil {
				return 0, err
			}
			continue
		}
		n, err := t.writeEvent(tx, e)
		if err != nil {
			return 0, err
		}
		inserted += n
	}
	if err := writeUsage(tx, usage); err != nil {
		return 0, err
	}
	for _, p := range f.st.Pending {
		if p.At.After(newest) {
			newest = p.At
		}
		if p.SessionID != "" && sessionID == "" {
			sessionID = p.SessionID
		}
	}
	if err := evictPending(tx, &f.st, newest); err != nil {
		return 0, err
	}
	if sessionID == "" {
		name := filepath.Base(path)
		sessionID = strings.TrimSuffix(name, filepath.Ext(name))
	}
	_, err = tx.Exec(`INSERT INTO transcript_offsets (path, session_id, byte_offset, inode, size, observer, sub, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, current_timestamp)
ON CONFLICT (path) DO UPDATE SET session_id = excluded.session_id, byte_offset = excluded.byte_offset,
  inode = excluded.inode, size = excluded.size, observer = excluded.observer, sub = excluded.sub,
  updated_at = excluded.updated_at`,
		path, sessionID, newOffset, inode, size, f.st.Observer, f.st.Sub)
	if err != nil {
		return 0, fmt.Errorf("save offset: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return inserted, nil
}

// requestSummary arms a summary request for the turn_end's session (R29, R32): a new or
// settled request becomes pending and due summaryDebounce after the turn_end (leading-edge:
// due_at is fixed by the first turn_end of a burst). A turn_end on an already pending
// request leaves due_at alone and bumps gen, so a summarize that read the session earlier
// cannot settle the request as covering this turn. Observer and subagent turn_ends, and
// ones without a session id or time, make none.
func requestSummary(tx *sql.Tx, e memdEvent) error {
	if e.SessionID == "" || e.At.IsZero() || e.Observer || e.Sub {
		return nil
	}
	_, err := tx.Exec(`INSERT INTO summary_requests (session_id, state, due_at, updated_at)
VALUES (?, 'pending', CAST(? AS TIMESTAMP), current_timestamp)
ON CONFLICT (session_id) DO UPDATE SET
  gen = CASE WHEN summary_requests.state = 'pending' THEN summary_requests.gen + 1 ELSE summary_requests.gen END,
  due_at = CASE WHEN summary_requests.state = 'pending' THEN summary_requests.due_at ELSE excluded.due_at END,
  attempts = CASE WHEN summary_requests.state = 'pending' THEN summary_requests.attempts ELSE 0 END,
  reason = CASE WHEN summary_requests.state = 'pending' THEN summary_requests.reason ELSE '' END,
  state = 'pending', updated_at = excluded.updated_at`, e.SessionID, memdTS(e.At.Add(summaryDebounce)))
	if err != nil {
		return fmt.Errorf("request summary: %w", err)
	}
	return nil
}

// writeEvent upserts the session and inserts the prompt or tool_use unless it is already
// there. It returns 1 when a row was inserted. A subagent's prompt is dropped (the session
// row is still touched). A tool_use gets a queue row in the same
// transaction, and only when the tool_uses row was new and the file is neither an observer's
// nor a subagent's.
func (t *memdTailer) writeEvent(tx *sql.Tx, e memdEvent) (int, error) {
	at := e.At.UTC()
	if e.At.IsZero() {
		at = time.Unix(0, 0).UTC()
	}
	seen := time.Now().UTC()
	if !e.At.IsZero() {
		seen = at
	}
	// Timestamps go in as UTC text and are cast in SQL: a time.Time parameter is converted
	// through the session time zone, which shifts stored values and breaks equality checks.
	atS, seenS := memdTS(at), memdTS(seen)
	if _, err := tx.Exec(`INSERT INTO sessions (session_id, project, cwd, is_observer, started_at, last_seen_at)
VALUES (?, ?, ?, ?, CAST(? AS TIMESTAMP), CAST(? AS TIMESTAMP))
ON CONFLICT (session_id) DO UPDATE SET
  is_observer = sessions.is_observer OR excluded.is_observer,
  last_seen_at = greatest(sessions.last_seen_at, excluded.last_seen_at)`,
		e.SessionID, e.Project, e.Cwd, e.Observer, seenS, seenS); err != nil {
		return 0, fmt.Errorf("upsert session: %w", err)
	}
	switch e.Kind {
	case "prompt":
		if e.Sub {
			return 0, nil // a subagent brief, not something the user typed
		}
		res, err := tx.Exec(`INSERT INTO prompts (session_id, prompt_number, text, created_at)
SELECT ?, 1 + (SELECT count(*) FROM prompts WHERE session_id = ?), ?, CAST(? AS TIMESTAMP)
WHERE NOT EXISTS (SELECT 1 FROM prompts WHERE session_id = ? AND created_at = CAST(? AS TIMESTAMP))`,
			e.SessionID, e.SessionID, e.PromptText, atS, e.SessionID, atS)
		if err != nil {
			return 0, fmt.Errorf("insert prompt: %w", err)
		}
		return rowsAffected(res), nil
	case "tool_use":
		res, err := tx.Exec(`INSERT INTO tool_uses (session_id, tool_use_id, tool_name, input, output, created_at)
VALUES (?, ?, ?, ?, ?, CAST(? AS TIMESTAMP)) ON CONFLICT (session_id, tool_use_id) DO NOTHING`,
			e.SessionID, e.ToolUseID, e.ToolName, e.Input, e.Output, atS)
		if err != nil {
			return 0, fmt.Errorf("insert tool_use: %w", err)
		}
		n := rowsAffected(res)
		if n > 0 && !e.Observer && !e.Sub {
			if _, err := tx.Exec(`INSERT INTO queue (session_id, tool_use_id, state) VALUES (?, ?, 'queued')`,
				e.SessionID, e.ToolUseID); err != nil {
				return 0, fmt.Errorf("enqueue: %w", err)
			}
		}
		return n, nil
	}
	return 0, nil
}

// memdTS renders t as UTC text for CAST(? AS TIMESTAMP).
func memdTS(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05.999999") }

func rowsAffected(res sql.Result) int {
	n, err := res.RowsAffected()
	if err != nil || n < 0 {
		return 0
	}
	return int(n)
}

// evictPending drops tool_uses that never got a result: those more than an hour older than
// the newest line seen in the file, then the oldest beyond memdPendingCap. Each one leaves a
// loss_receipts row.
func evictPending(tx *sql.Tx, st *memdFileState, newest time.Time) error {
	var drop []string
	if !newest.IsZero() {
		for id, p := range st.Pending {
			if newest.Sub(p.At) > memdPendingMaxAge {
				drop = append(drop, id)
			}
		}
	}
	gone := map[string]bool{}
	for _, id := range drop {
		gone[id] = true
	}
	if over := len(st.Pending) - len(drop) - memdPendingCap; over > 0 {
		var rest []string
		for id := range st.Pending {
			if !gone[id] {
				rest = append(rest, id)
			}
		}
		sort.Slice(rest, func(i, j int) bool {
			a, b := st.Pending[rest[i]], st.Pending[rest[j]]
			if !a.At.Equal(b.At) {
				return a.At.Before(b.At)
			}
			return rest[i] < rest[j]
		})
		drop = append(drop, rest[:over]...)
	}
	for _, id := range drop {
		p := st.Pending[id]
		if _, err := tx.Exec(`INSERT INTO loss_receipts (kind, session_id, detail) VALUES ('pending_evicted', ?, ?)`,
			p.SessionID, p.ToolName); err != nil {
			return fmt.Errorf("loss receipt: %w", err)
		}
		delete(st.Pending, id)
	}
	return nil
}

// Run scans once, then again on every filesystem event (debounced) and every 5 s as a
// fallback for events fsnotify missed. It returns nil when ctx is cancelled.
func (t *memdTailer) Run(ctx context.Context) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("fsnotify: %w", err)
	}
	defer w.Close()
	for root := range t.roots {
		watchTree(w, root)
	}
	go func() { // history is read once, off the live path
		if err := t.BackfillUsage(ctx); err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "memory usage backfill:", err)
		}
	}()
	if _, err := t.Scan(); err != nil {
		fmt.Fprintln(os.Stderr, "memory tail:", err)
	}
	tick := time.NewTicker(memdScanEvery)
	defer tick.Stop()
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	defer debounce.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			if ev.Has(fsnotify.Create) {
				if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
					watchTree(w, ev.Name)
				}
			}
			debounce.Reset(memdEventDebounce)
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			fmt.Fprintln(os.Stderr, "memory tail watcher:", err)
		case <-debounce.C:
			t.scanLogged()
		case <-tick.C:
			t.scanLogged()
		}
	}
}

func (t *memdTailer) scanLogged() {
	if _, err := t.Scan(); err != nil {
		fmt.Fprintln(os.Stderr, "memory tail:", err)
	}
}

// watchTree watches dir and every directory below it. Missing dirs are skipped; the 5 s
// scan still covers them.
func watchTree(w *fsnotify.Watcher, dir string) {
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			w.Add(p)
		}
		return nil
	})
}
