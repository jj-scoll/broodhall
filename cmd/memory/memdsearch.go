package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// memory keyword search. Two paths score with the same analyzer and BM25F formula:
//
//   - mem: an in-memory inverted index, built in the background at daemon start and updated
//     synchronously on every write (the primary path);
//   - sql: postings tables in DuckDB (<prefix>_docs/_dict/_post), kept current on every write
//     and backfilled on open, scored with one aggregate query (used while mem is building).
//
// A search never waits for the build. A row inserted behind memory's back (a test, a
// direct SQL insert) is caught up on the next search: the row count of the table is compared with the
// postings table's, which costs two O(1) counts.

// memdIndexBuildHook is a test hook called after a build's scan and before it is installed.
var memdIndexBuildHook func()

type kindState struct {
	k   *searchKind
	mem atomic.Pointer[memIndex]

	mu       sync.Mutex // building, pending, lastMiss
	building bool
	pending  []*analyzedDoc // documents written during a build
	lastMiss [2]int64       // last (table, postings) counts seen to differ

	wmu      sync.Mutex // serializes writes to the postings tables
	nextTerm int32
	haveNext bool
}

type memdSearcher struct {
	db *sql.DB
	st map[*searchKind]*kindState
}

var searchers sync.Map // *sql.DB -> *memdSearcher

// searcherFor returns db's search state, creating it on first use.
func searcherFor(db *sql.DB) *memdSearcher {
	if v, ok := searchers.Load(db); ok {
		return v.(*memdSearcher)
	}
	s := &memdSearcher{db: db, st: map[*searchKind]*kindState{}}
	for _, k := range searchKinds {
		s.st[k] = &kindState{k: k}
	}
	v, _ := searchers.LoadOrStore(db, s)
	return v.(*memdSearcher)
}

// searchersReset forgets db's search state (tests that rewrite the postings tables).
func searchersReset(db *sql.DB) { searchers.Delete(db) }

// pathFor names the path a search of k takes now.
func (s *memdSearcher) pathFor(k *searchKind) string {
	if s.st[k].mem.Load() != nil {
		return "mem"
	}
	return "sql"
}

// buildAll builds the in-memory index of every kind, observations first. It returns when
// done; run it in a goroutine. Failures are logged and leave that kind on the SQL path.
func (s *memdSearcher) buildAll(ctx context.Context) {
	for _, k := range searchKinds {
		t0 := time.Now()
		if err := s.buildMem(ctx, k); err != nil {
			log.Printf("memory search: in-memory %s index not built, using SQL: %v", k.name, err)
			continue
		}
		log.Printf("memory search: in-memory %s index ready: %d docs in %v", k.name, s.st[k].mem.Load().numDocs(), time.Since(t0).Round(time.Millisecond))
	}
}

// buildMem scans k's table into a new in-memory index and installs it. Writes that land
// during the scan are queued and applied before the install, so none is lost.
func (s *memdSearcher) buildMem(ctx context.Context, k *searchKind) error {
	ks := s.st[k]
	ks.mu.Lock()
	ks.building, ks.pending = true, nil
	ks.mu.Unlock()
	ix, err := buildMemFromDB(ctx, s.db, k)
	if err == nil && memdIndexBuildHook != nil {
		memdIndexBuildHook()
	}
	ks.mu.Lock()
	defer ks.mu.Unlock()
	pending := ks.pending
	ks.building, ks.pending = false, nil
	if err != nil {
		return err
	}
	for _, d := range pending {
		if _, dup := ix.byID[d.id]; !dup {
			ix.add(d)
		}
	}
	ks.mem.Store(ix)
	return nil
}

// toMem applies one written document to the in-memory index, or queues it during a build.
func (ks *kindState) toMem(d *analyzedDoc) {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if ix := ks.mem.Load(); ix != nil {
		ix.addLocked(d)
	} else if ks.building {
		ks.pending = append(ks.pending, d)
	}
}

// rank returns the ids matching q, best first, for rows [offset, offset+limit) of the ranking.
// path is "mem", "sql", or "" for whichever is ready.
func (s *memdSearcher) rank(k *searchKind, q string, f qFilter, limit, offset int, path string) ([]int64, error) {
	terms := queryTerms(q)
	if len(terms) == 0 || limit <= 0 {
		return nil, nil
	}
	if err := s.catchUp(k); err != nil {
		return nil, err
	}
	if path == "" {
		path = s.pathFor(k)
	}
	want := limit + offset
	var ids []int64
	switch path {
	case "mem":
		ix := s.st[k].mem.Load()
		if ix == nil {
			return nil, fmt.Errorf("in-memory %s index not built", k.name)
		}
		ids = ix.search(terms, f, want)
	case "sql":
		var err error
		if ids, err = s.rankSQL(k, terms, f, want); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown search path %q", path)
	}
	if offset >= len(ids) {
		return nil, nil
	}
	return ids[offset:], nil
}

// catchUp indexes rows the write hooks never saw. Cheap when nothing is missing.
func (s *memdSearcher) catchUp(k *searchKind) error {
	var nSrc, nIdx int64
	if err := s.db.QueryRow(fmt.Sprintf(`SELECT (SELECT count(*) FROM %s), (SELECT count(*) FROM %s_docs)`, k.table, k.prefix)).Scan(&nSrc, &nIdx); err != nil {
		return fmt.Errorf("search index check: %w", err)
	}
	ks := s.st[k]
	if nSrc == nIdx {
		return nil
	}
	ks.mu.Lock()
	seen := ks.lastMiss == [2]int64{nSrc, nIdx}
	ks.lastMiss = [2]int64{nSrc, nIdx}
	ks.mu.Unlock()
	if seen {
		return nil // rows were deleted behind our back; nothing to add
	}
	return withWrite(func() error { return s.indexMissing(context.Background(), k) })
}

// memdCatchUpBulk is the number of missing rows above which the postings are rebuilt in bulk.
const memdCatchUpBulk = 1000

// indexMissing adds every row of k's table that is not yet in the postings tables.
func (s *memdSearcher) indexMissing(ctx context.Context, k *searchKind) error {
	var missing int64
	where := fmt.Sprintf(" FROM %s WHERE id NOT IN (SELECT id FROM %s_docs)", k.table, k.prefix)
	if err := s.db.QueryRow(`SELECT count(*)` + where).Scan(&missing); err != nil {
		return fmt.Errorf("search index catch-up: %w", err)
	}
	if missing == 0 {
		return nil
	}
	if missing > memdCatchUpBulk {
		return s.backfillFull(ctx, k)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+k.selectCols()+where)
	if err != nil {
		return fmt.Errorf("search index catch-up: %w", err)
	}
	var docs []rawDoc
	for rows.Next() {
		var d rawDoc
		if err := rows.Scan(d.dests(k)...); err != nil {
			rows.Close()
			return err
		}
		docs = append(docs, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	a := newMemdAnalyzer()
	for i := range docs {
		if err := s.indexDoc(k, analyzeDoc(a, k, &docs[i])); err != nil {
			return err
		}
	}
	return nil
}

// indexDoc writes one analyzed document to the postings tables (unless present) and to memory.
func (s *memdSearcher) indexDoc(k *searchKind, d *analyzedDoc) error {
	if err := s.writeDocSQL(k, d); err != nil {
		return err
	}
	s.st[k].toMem(d)
	return nil
}

// indexRow is the write hook: read row id back from the table and index it. Failures are
// not fatal to the write; the next search's catch-up repairs the gap.
func (s *memdSearcher) indexRow(k *searchKind, id int64) error {
	var d rawDoc
	err := s.db.QueryRow("SELECT "+k.selectCols()+" FROM "+k.table+" WHERE id = ?", id).Scan(d.dests(k)...)
	if err != nil {
		return fmt.Errorf("read %s %d: %w", k.table, id, err)
	}
	return s.indexDoc(k, analyzeDoc(newMemdAnalyzer(), k, &d))
}

// memdIndexObservation and memdIndexSummary are called after a row is committed.
func memdIndexObservation(db *sql.DB, id int64) { memdIndexRow(db, obsKind, id) }
func memdIndexSummary(db *sql.DB, id int64)     { memdIndexRow(db, sumKind, id) }

func memdIndexRow(db *sql.DB, k *searchKind, id int64) {
	if err := searcherFor(db).indexRow(k, id); err != nil {
		log.Printf("memory search: index %s %d (the next search catches up): %v", k.name, id, err)
	}
}

// hydrateIDs loads rows by id in the order given.
func hydrateObs(db *sql.DB, ids []int64) ([]*qObs, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph, args := inList(ids)
	rows, err := qObsQuery(db, `SELECT `+obsCols+` FROM observations WHERE id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]*qObs, len(rows))
	for _, o := range rows {
		byID[o.id] = o
	}
	out := make([]*qObs, 0, len(ids))
	for _, id := range ids {
		if o := byID[id]; o != nil {
			out = append(out, o)
		}
	}
	return out, nil
}

func hydrateSummaries(db *sql.DB, ids []int64) ([]*qSummary, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph, args := inList(ids)
	rows, err := db.Query(`SELECT `+sumCols+` FROM summaries WHERE id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sums, err := scanSummaries(rows)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]*qSummary, len(sums))
	for _, s := range sums {
		byID[s.id] = s
	}
	out := make([]*qSummary, 0, len(ids))
	for _, id := range ids {
		if s := byID[id]; s != nil {
			out = append(out, s)
		}
	}
	return out, nil
}

func inList(ids []int64) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}

// countHits is len(rank(...)) without scoring on the SQL path, for callers that display only
// the number of hits.
func (s *memdSearcher) countHits(k *searchKind, q string, f qFilter, limit, offset int) (int, error) {
	return s.countHitsOn(k, q, f, limit, offset, "")
}

func (s *memdSearcher) countHitsOn(k *searchKind, q string, f qFilter, limit, offset int, path string) (int, error) {
	terms := queryTerms(q)
	if len(terms) == 0 || limit <= 0 {
		return 0, nil
	}
	if err := s.catchUp(k); err != nil {
		return 0, err
	}
	if path == "" {
		path = s.pathFor(k)
	}
	if path == "mem" {
		ids, err := s.rank(k, q, f, limit, offset, "mem")
		return len(ids), err
	}
	n, err := s.countSQL(k, terms, f)
	if err != nil {
		return 0, err
	}
	return min(max(n-offset, 0), limit), nil
}
