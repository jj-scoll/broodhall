package main

// memory import: read every past transcript under the tailer's roots into the database, so
// old sessions are searchable and resumable. It runs inside the daemon (which holds the DB
// lock) in the background, one file at a time, in small transactions.
//
// Rows go through the tailer's own parser, masking, capture rules and writeEvent, with three
// differences, all in memdTailer.importing: no summary request, no queue row (the observer
// never sees history), and transcript_offsets is only ever inserted, never moved.
//
// The tailer owns whatever is not yet behind its read position. The import reads each file
// only up to that position (memdTailer.frontier), so a line the tailer has yet to read is
// never written first by the import: the tailer would then find the row present, add
// nothing, and queue nothing. Files the tailer has not seen yet wait for the next run.
//
// Progress per file is in import_files (offset, inode, limit, latches), so a re-run reads
// only what grew. A file that ended with a tool call still waiting for its result is read
// again from 0 when it grows (the waiting call is not kept between runs); every write is
// idempotent.

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var errImportRunning = errors.New("an import is already running")

const (
	// importChunkBytes and importChunkLines bound one transaction, and with it the memory
	// held per file (a 100 MB transcript is read 4 MB at a time) and how long the write
	// lock is held.
	importChunkBytes = 4 << 20
	// importIndexEvery is how many new rows to collect before the raw index is brought up
	// to date; the rest happens at the end of the run.
	importIndexEvery = 2000
	// importPeekLines is how many lines are read from a file's head to find when it opened.
	importPeekLines = 40
)

var (
	importChunkLines = 2000
	// importAfterChunk, when set (tests), runs after each committed chunk.
	importAfterChunk func()
	// memdActiveImporter is the running daemon's importer (nil when none).
	memdActiveImporter atomic.Pointer[memdImporter]
)

// importStatus is what GET /import reports.
type importStatus struct {
	Running       bool      `json:"running"`
	DryRun        bool      `json:"dry_run"`
	FilesTotal    int       `json:"files_total"`    // files to read (or read, when done)
	FilesDone     int       `json:"files_done"`     // of those, finished
	FilesCurrent  int       `json:"files_current"`  // already imported, nothing new
	FilesDeferred int       `json:"files_deferred"` // the tailer has not seen them yet
	FilesFailed   int       `json:"files_failed"`
	BytesTotal    int64     `json:"bytes_total"`
	BytesDone     int64     `json:"bytes_done"`
	Rows          int       `json:"rows"` // prompts, replies and tool calls newly added
	Started       time.Time `json:"started"`
	Finished      time.Time `json:"finished"`
	Err           string    `json:"error,omitempty"`
}

type memdImporter struct {
	db       *sql.DB
	roots    map[string]string
	frontier func(path string) (int64, bool) // nil: read whole files
	w        *memdTailer                     // the tailer's write path, in importing mode
	ctx      context.Context                 // the daemon's; Background when unset

	mu   sync.Mutex
	st   importStatus
	done chan struct{}
}

// newMemdImporter returns an importer over roots (root dir -> account). frontier is the
// tailer's read position of a path (see memdTailer.frontier); nil reads every file whole.
func newMemdImporter(db *sql.DB, roots map[string]string, frontier func(path string) (int64, bool)) *memdImporter {
	return &memdImporter{db: db, roots: roots, frontier: frontier,
		w: &memdTailer{db: db, roots: roots, importing: true}, ctx: context.Background()}
}

// Start begins an import in the background, or a dry run that only counts. It returns
// errImportRunning when one is in progress.
func (m *memdImporter) Start(ctx context.Context, dry bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.st.Running {
		return errImportRunning
	}
	m.st = importStatus{Running: true, DryRun: dry, Started: time.Now()}
	m.done = make(chan struct{})
	go m.run(ctx, dry, m.done)
	return nil
}

// Status is a snapshot of the current or last run.
func (m *memdImporter) Status() importStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st
}

// Wait blocks until the current run, if any, has finished.
func (m *memdImporter) Wait() {
	m.mu.Lock()
	d := m.done
	m.mu.Unlock()
	if d != nil {
		<-d
	}
}

func (m *memdImporter) update(f func(*importStatus)) {
	m.mu.Lock()
	f(&m.st)
	m.mu.Unlock()
}

func (m *memdImporter) run(ctx context.Context, dry bool, done chan struct{}) {
	defer close(done)
	err := m.runAll(ctx, dry)
	m.update(func(s *importStatus) {
		s.Running = false
		s.Finished = time.Now()
		if err != nil {
			s.Err = err.Error()
		}
	})
	if err != nil {
		log.Printf("memory import: %v", err)
	} else if !dry {
		st := m.Status()
		log.Printf("memory import: %d files, %d bytes, %d rows added in %v (%d deferred, %d failed)",
			st.FilesDone, st.BytesDone, st.Rows, st.Finished.Sub(st.Started).Round(time.Second), st.FilesDeferred, st.FilesFailed)
	}
}

// importProgress is a row of import_files.
type importProgress struct {
	offset, inode, limit int64
	observer, sub, open  bool
}

// importWork is one file to read: bytes [start, limit) with the latches as they were at start.
type importWork struct {
	path, account string
	inode, size   int64
	start, limit  int64
	st            memdFileState
	opened        time.Time
}

func (m *memdImporter) loadProgress() (map[string]importProgress, error) {
	rows, err := m.db.Query(`SELECT path, byte_offset, inode, size, observer, sub, open_tools FROM import_files`)
	if err != nil {
		return nil, fmt.Errorf("load import progress: %w", err)
	}
	defer rows.Close()
	out := map[string]importProgress{}
	for rows.Next() {
		var path string
		var p importProgress
		if err := rows.Scan(&path, &p.offset, &p.inode, &p.limit, &p.observer, &p.sub, &p.open); err != nil {
			return nil, err
		}
		out[path] = p
	}
	return out, rows.Err()
}

// plan lists the files with something to read, original sessions before their forks.
func (m *memdImporter) plan(ctx context.Context) ([]importWork, error) {
	prog, err := m.loadProgress()
	if err != nil {
		return nil, err
	}
	var work []importWork
	roots := make([]string, 0, len(m.roots))
	for r := range m.roots {
		roots = append(roots, r)
	}
	sort.Strings(roots)
	for _, root := range roots {
		filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if werr != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
			fi, err := os.Stat(path)
			if err != nil {
				return nil
			}
			w := importWork{path: path, account: m.roots[root], size: fi.Size(), limit: fi.Size()}
			if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
				w.inode = int64(sys.Ino)
			}
			if m.frontier != nil {
				fr, ok := m.frontier(path)
				if !ok {
					m.update(func(s *importStatus) { s.FilesDeferred++ })
					return nil
				}
				w.limit = min(w.limit, fr)
			}
			p, seen := prog[path]
			if seen && p.inode == w.inode && p.offset <= w.limit {
				if p.limit == w.limit {
					m.update(func(s *importStatus) { s.FilesCurrent++ })
					return nil
				}
				if !p.open {
					w.start = p.offset
					w.st.Observer, w.st.Sub = p.observer, p.sub
				}
			}
			if w.limit == 0 && !seen {
				m.update(func(s *importStatus) { s.FilesCurrent++ })
				return nil
			}
			w.opened = peekOpened(path)
			work = append(work, w)
			return nil
		})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// A fork is linked to the earliest-opened session of its family as it is written, so the
	// original goes first.
	sort.SliceStable(work, func(i, j int) bool {
		a, b := work[i].opened, work[j].opened
		if a.IsZero() != b.IsZero() {
			return !a.IsZero()
		}
		if !a.Equal(b) {
			return a.Before(b)
		}
		return work[i].path < work[j].path
	})
	return work, nil
}

// peekOpened is the time the transcript was opened: its first timestamp.
func peekOpened(path string) time.Time {
	fh, err := os.Open(path)
	if err != nil {
		return time.Time{}
	}
	defer fh.Close()
	br := bufio.NewReaderSize(io.LimitReader(fh, 1<<20), 64<<10)
	var st memdFileState
	for i := 0; i < importPeekLines && st.OpenedAt.IsZero(); i++ {
		line, err := br.ReadBytes('\n')
		if v, derr := decodeJSON(line); derr == nil {
			if d, ok := v.(*obj); ok {
				noteForkFacts(d, &st)
			}
		}
		if err != nil {
			break
		}
	}
	return st.OpenedAt
}

func (m *memdImporter) runAll(ctx context.Context, dry bool) error {
	work, err := m.plan(ctx)
	if err != nil {
		return err
	}
	var total int64
	for _, w := range work {
		total += w.limit - w.start
	}
	m.update(func(s *importStatus) { s.FilesTotal, s.BytesTotal = len(work), total })
	if dry {
		return nil
	}
	unindexed := 0
	for _, w := range work {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := m.importFile(ctx, w)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("memory import: %s: %v", w.path, err)
			m.update(func(s *importStatus) { s.FilesFailed++ })
			continue
		}
		m.update(func(s *importStatus) { s.FilesDone++ })
		if unindexed += n; unindexed >= importIndexEvery {
			unindexed = 0
			if err := rawIndexAll(ctx, m.db); err != nil {
				return fmt.Errorf("raw index: %w", err)
			}
		}
	}
	if unindexed > 0 {
		if err := rawIndexAll(ctx, m.db); err != nil {
			return fmt.Errorf("raw index: %w", err)
		}
	}
	return nil
}

// importFile reads bytes [w.start, w.limit) of one transcript in chunks, one transaction
// each, and returns how many rows were newly added.
func (m *memdImporter) importFile(ctx context.Context, w importWork) (added int, err error) {
	fh, err := os.Open(w.path)
	if err != nil {
		return 0, nil // vanished
	}
	defer fh.Close()
	br := bufio.NewReaderSize(io.NewSectionReader(fh, w.start, w.limit-w.start), 1<<20)
	w.st.forkKnown = w.start > 0
	f := &memdTailFile{st: w.st, known: true, resumed: true, offset: w.start, inode: w.inode, size: w.limit}
	pos := w.start // end of the last whole line read
	chunkFrom := pos
	var lines [][]byte
	var chunkBytes int
	flush := func(final bool) error {
		if len(lines) == 0 && !final {
			return nil
		}
		events := parseMemdLines(w.path, w.account, lines, &f.st)
		f.importFinal = final
		var n int
		err := withWrite(func() error {
			var cerr error
			n, cerr = m.w.commit(w.path, f, events, w.inode, pos, w.size)
			return cerr
		})
		if err != nil {
			return err
		}
		added += n
		f.offset = pos
		lines, chunkBytes = lines[:0], 0
		read := pos - chunkFrom
		chunkFrom = pos
		m.update(func(s *importStatus) { s.BytesDone += read; s.Rows += n })
		if importAfterChunk != nil {
			importAfterChunk()
		}
		return ctx.Err()
	}
	for {
		line, rerr := br.ReadBytes('\n')
		if n := len(line); n > 0 && line[n-1] == '\n' {
			pos += int64(n)
			chunkBytes += n
			if line = bytes.TrimRight(line, "\r\n"); len(line) > 0 {
				lines = append(lines, line)
			}
			if len(lines) >= importChunkLines || chunkBytes >= importChunkBytes {
				if err := flush(false); err != nil {
					return added, err
				}
			}
		}
		if rerr != nil { // EOF: a last line without its newline is still being written
			if rerr != io.EOF {
				return added, rerr
			}
			break
		}
	}
	return added, flush(true)
}

// saveImportProgress records how far the import has read. At the end of a file it also
// inserts the file's transcript_offsets row if the tailer has none, so the session can be
// resumed and a restarted tailer carries on from where the import stopped; an existing row
// is the tailer's and is left alone.
func saveImportProgress(tx *sql.Tx, path, sessionID string, f *memdTailFile, inode, offset, size int64) error {
	if _, err := tx.Exec(`INSERT INTO import_files (path, byte_offset, inode, size, observer, sub, open_tools, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, current_timestamp)
ON CONFLICT (path) DO UPDATE SET byte_offset = excluded.byte_offset, inode = excluded.inode, size = excluded.size,
  observer = excluded.observer, sub = excluded.sub, open_tools = excluded.open_tools, updated_at = excluded.updated_at`,
		path, offset, inode, f.size, f.st.Observer, f.st.Sub, len(f.st.Pending) > 0); err != nil {
		return fmt.Errorf("save import progress: %w", err)
	}
	if !f.importFinal {
		return nil
	}
	// A session the live tailer joined late has its later prompts numbered first. Only rows
	// whose number is wrong are touched, so a session that was only imported costs a read.
	if _, err := tx.Exec(`UPDATE prompts SET prompt_number = r.rn
FROM (SELECT id, row_number() OVER (ORDER BY created_at, id) AS rn FROM prompts WHERE session_id = ?) r
WHERE prompts.id = r.id AND prompts.prompt_number <> r.rn`, sessionID); err != nil {
		return fmt.Errorf("renumber prompts: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO transcript_offsets (path, session_id, byte_offset, inode, size, observer, sub, reset_kind, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, current_timestamp) ON CONFLICT (path) DO NOTHING`,
		path, sessionID, offset, inode, size, f.st.Observer, f.st.Sub, f.st.Reset); err != nil {
		return fmt.Errorf("save offset: %w", err)
	}
	return nil
}

// importSession is what one chunk of events says about one session.
type importSession struct {
	project, cwd  string
	observer      bool
	first, last   time.Time
	branch, model string
}

// noteImportSession folds an event into its session's entry. A chunk then writes each session
// once, with the result the per-event upsert and branch/model update give live: the project
// and directory of the first event, the observer flag if any event had it, the newest time,
// and the last branch and model seen outside subagent events. Updating the one sessions row
// for every event was most of the import's time.
func (t *memdTailer) noteImportSession(e memdEvent, seen time.Time) {
	if t.impSessions == nil {
		t.impSessions = map[string]*importSession{}
	}
	s := t.impSessions[e.SessionID]
	if s == nil {
		s = &importSession{project: e.Project, cwd: e.Cwd, first: seen, last: seen}
		t.impSessions[e.SessionID] = s
	}
	s.observer = s.observer || e.Observer
	if seen.After(s.last) {
		s.last = seen
	}
	if !e.Sub {
		if e.GitBranch != "" {
			s.branch = e.GitBranch
		}
		if e.Model != "" {
			s.model = e.Model
		}
	}
}

// flushImportSessions writes the sessions noted since the chunk began.
func (t *memdTailer) flushImportSessions(tx *sql.Tx) error {
	for id, s := range t.impSessions {
		if _, err := tx.Exec(`INSERT INTO sessions (session_id, project, cwd, is_observer, started_at, last_seen_at, git_branch, model)
VALUES (?, ?, ?, ?, CAST(? AS TIMESTAMP), CAST(? AS TIMESTAMP), ?, ?)
ON CONFLICT (session_id) DO UPDATE SET
  is_observer = sessions.is_observer OR excluded.is_observer,
  started_at = least(sessions.started_at, excluded.started_at),
  last_seen_at = greatest(sessions.last_seen_at, excluded.last_seen_at),
  git_branch = coalesce(nullif(excluded.git_branch, ''), sessions.git_branch),
  model = coalesce(nullif(excluded.model, ''), sessions.model)`,
			id, s.project, s.cwd, s.observer, memdTS(s.first), memdTS(s.last), s.branch, s.model); err != nil {
			return fmt.Errorf("upsert session: %w", err)
		}
	}
	t.impSessions = nil
	return nil
}

// frontier is how far the tailer has read path: the end of the last whole line it consumed,
// or the end of the file for one it skipped as old. ok is false for a file it has not seen.
func (t *memdTailer) frontier(path string) (int64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f := t.files[path]
	if f == nil || !f.known {
		return 0, false
	}
	return f.offset, true
}

// memdMigrateImport creates import_files, the import's own per-file progress.
func memdMigrateImport(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS import_files (
	path        VARCHAR PRIMARY KEY,
	byte_offset BIGINT NOT NULL,
	inode       BIGINT NOT NULL,
	size        BIGINT NOT NULL,
	observer    BOOLEAN NOT NULL DEFAULT false,
	sub         BOOLEAN NOT NULL DEFAULT false,
	open_tools  BOOLEAN NOT NULL DEFAULT false,
	updated_at  TIMESTAMP NOT NULL DEFAULT current_timestamp
)`); err != nil {
		return fmt.Errorf("memory schema: import_files: %w", err)
	}
	return nil
}
