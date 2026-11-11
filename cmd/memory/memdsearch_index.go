package main

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"runtime"
	"strings"
	"sync"
)

// The in-memory BM25F index (the primary search path). Documents are analyzed by the same
// analyzer as the SQL postings tables, so both paths rank identically (TestMemdPathParity).

const (
	maxFields = 6
	// BM25F parameters from the quality study (agent-reports/memory-search-quality.md, variant F3).
	bm25K = 2.0
	bm25B = 0.5
)

// searchKind describes one searchable table: its indexed columns (field order) and weights.
type searchKind struct {
	name   string
	table  string
	prefix string // postings tables: <prefix>_docs, <prefix>_dict, <prefix>_post
	cols   []string
	w      []float64
	typed  bool // the table has a type column
}

var obsKind = &searchKind{"obs", "observations", "ox",
	[]string{"title", "subtitle", "narrative", "facts", "concepts"}, []float64{3, 2, 1, 1, 0.5}, true}

// Summary weights follow the same shape (the request is the title) but are not tuned: the
// quality set only covers observations.
var sumKind = &searchKind{"sum", "summaries", "sx",
	[]string{"request", "investigated", "learned", "completed", "next_steps", "notes"}, []float64{3, 1, 1, 1, 0.5, 0.5}, false}

var searchKinds = []*searchKind{obsKind, sumKind}

func (k *searchKind) selectCols() string {
	typ := "''"
	if k.typed {
		typ = "type"
	}
	return "id, project, " + typ + ", epoch_us(created_at), " + strings.Join(k.cols, ", ")
}

// rawDoc is one table row as the analyzer sees it.
type rawDoc struct {
	id      int64
	project string
	typ     string
	created int64 // microseconds since the epoch, UTC
	f       [maxFields]string
}

// dests lists the scan targets matching k.selectCols().
func (d *rawDoc) dests(k *searchKind) []any {
	dst := []any{&d.id, &d.project, &d.typ, &d.created}
	for i := range k.cols {
		dst = append(dst, &d.f[i])
	}
	return dst
}

type docTerm struct {
	term string
	tf   [maxFields]uint8
}

type analyzedDoc struct {
	id      int64
	project string
	typ     string
	created int64
	lens    [maxFields]int32
	terms   []docTerm
}

func analyzeDoc(a *memdAnalyzer, k *searchKind, d *rawDoc) *analyzedDoc {
	ad := &analyzedDoc{id: d.id, project: d.project, typ: d.typ, created: d.created}
	pos := make(map[string]int, 64)
	for fi := range k.cols {
		for _, tk := range a.analyze(d.f[fi]) {
			j, ok := pos[tk.term]
			if !ok {
				j = len(ad.terms)
				pos[tk.term] = j
				ad.terms = append(ad.terms, docTerm{term: tk.term})
			}
			if ad.terms[j].tf[fi] < 255 {
				ad.terms[j].tf[fi]++
			}
			if tk.whole {
				ad.lens[fi]++
			}
		}
	}
	return ad
}

// queryTerms analyzes a query and drops duplicate terms.
func queryTerms(q string) []string {
	a := &memdAnalyzer{stems: make(map[string]string, 16)}
	seen := map[string]bool{}
	var out []string
	for _, tk := range a.analyze(q) {
		if !seen[tk.term] {
			seen[tk.term] = true
			out = append(out, tk.term)
		}
	}
	return out
}

type memPosting struct {
	doc int32
	tf  [maxFields]uint8
}

type memDoc struct {
	id      int64
	created int64
	project int32
	typ     int32
	lens    [maxFields]int32
}

type memScratch struct {
	acc     []float64
	stamp   []uint32
	epoch   uint32
	touched []int32
}

type memIndex struct {
	k      *searchKind
	mu     sync.RWMutex
	vocab  map[string]int32
	post   [][]memPosting
	docs   []memDoc
	byID   map[int64]int32
	lenSum [maxFields]int64
	strs   map[string]int32
	pool   sync.Pool
}

func newMemIndex(k *searchKind) *memIndex {
	return &memIndex{k: k, vocab: map[string]int32{}, byID: map[int64]int32{}, strs: map[string]int32{}}
}

func (ix *memIndex) intern(s string) int32 {
	if id, ok := ix.strs[s]; ok {
		return id
	}
	id := int32(len(ix.strs))
	ix.strs[strings.Clone(s)] = id
	return id
}

// add appends a document. The caller owns ix exclusively or holds ix.mu for writing.
func (ix *memIndex) add(d *analyzedDoc) {
	idx := int32(len(ix.docs))
	ix.docs = append(ix.docs, memDoc{id: d.id, created: d.created, project: ix.intern(d.project), typ: ix.intern(d.typ), lens: d.lens})
	ix.byID[d.id] = idx
	for fi := range d.lens {
		ix.lenSum[fi] += int64(d.lens[fi])
	}
	for i := range d.terms {
		t := &d.terms[i]
		id, ok := ix.vocab[t.term]
		if !ok {
			id = int32(len(ix.post))
			ix.vocab[strings.Clone(t.term)] = id
			ix.post = append(ix.post, nil)
		}
		ix.post[id] = append(ix.post[id], memPosting{idx, t.tf})
	}
}

// addLocked adds d unless its id is already indexed.
func (ix *memIndex) addLocked(d *analyzedDoc) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if _, dup := ix.byID[d.id]; !dup {
		ix.add(d)
	}
}

// compact moves every posting list into one exact-size backing array, dropping append slack.
func (ix *memIndex) compact() {
	total := 0
	for _, p := range ix.post {
		total += len(p)
	}
	big := make([]memPosting, total)
	off := 0
	for i, p := range ix.post {
		n := copy(big[off:], p)
		ix.post[i] = big[off : off+n : off+n]
		off += n
	}
}

func (ix *memIndex) numDocs() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.docs)
}

// scoreKey makes scores comparable across the two paths: rounded to 1e-9, as an integer.
func scoreKey(s float64) int64 { return int64(math.Round(s * 1e9)) }

type memHit struct {
	key     int64
	created int64
	id      int64
}

func (a memHit) before(b memHit) bool {
	if a.key != b.key {
		return a.key > b.key
	}
	if a.created != b.created {
		return a.created > b.created
	}
	return a.id > b.id
}

// search scores the analyzed query terms and returns the ids of the best want documents,
// ordered by score, then created_at, then id (all descending).
func (ix *memIndex) search(terms []string, f qFilter, want int) []int64 {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	n := len(ix.docs)
	if n == 0 || want <= 0 {
		return nil
	}
	// Filters resolve to interned ids once.
	var projIDs []int32
	if f.project != "" {
		for _, p := range f.projKeys() {
			if id, ok := ix.strs[p]; ok {
				projIDs = append(projIDs, id)
			}
		}
		if len(projIDs) == 0 {
			return nil
		}
	}
	var typeIDs []int32
	if ix.k.typed && len(f.types) > 0 {
		for _, t := range f.types {
			if id, ok := ix.strs[t]; ok {
				typeIDs = append(typeIDs, id)
			}
		}
		if len(typeIDs) == 0 {
			return nil
		}
	}
	var lo, hi int64 = math.MinInt64, math.MaxInt64
	if f.start != nil {
		lo = f.start.UTC().UnixMicro()
	}
	if f.end != nil {
		hi = f.end.UTC().UnixMicro()
	}
	filtered := projIDs != nil || typeIDs != nil || f.start != nil || f.end != nil

	sc, _ := ix.pool.Get().(*memScratch)
	if sc == nil {
		sc = &memScratch{}
	}
	if len(sc.acc) < n {
		sc.acc = make([]float64, n+n/4)
		sc.stamp = make([]uint32, n+n/4)
		sc.epoch = 0
	}
	sc.epoch++
	if sc.epoch == 0 {
		clear(sc.stamp)
		sc.epoch = 1
	}
	touched := sc.touched[:0]

	nf := len(ix.k.cols)
	c1 := 1 - bm25B
	var c2 [maxFields]float64
	for fi := 0; fi < nf; fi++ {
		avg := float64(ix.lenSum[fi]) / float64(n)
		if avg == 0 {
			avg = 1
		}
		c2[fi] = bm25B / avg
	}
	var w [maxFields]float64
	copy(w[:], ix.k.w)
	N := float64(n)

	for _, term := range terms {
		tid, ok := ix.vocab[term]
		if !ok {
			continue
		}
		pl := ix.post[tid]
		df := float64(len(pl))
		idf := math.Log(1 + (N-df+0.5)/(df+0.5))
		for i := range pl {
			p := &pl[i]
			d := &ix.docs[p.doc]
			if filtered {
				if projIDs != nil && !containsInt32(projIDs, d.project) || d.created < lo || d.created > hi {
					continue
				}
				if typeIDs != nil && !containsInt32(typeIDs, d.typ) {
					continue
				}
			}
			var ptf float64
			for fi := 0; fi < nf; fi++ {
				if tf := p.tf[fi]; tf != 0 {
					ptf += w[fi] * float64(tf) / (c1 + c2[fi]*float64(d.lens[fi]))
				}
			}
			if sc.stamp[p.doc] != sc.epoch {
				sc.stamp[p.doc] = sc.epoch
				sc.acc[p.doc] = 0
				touched = append(touched, p.doc)
			}
			sc.acc[p.doc] += idf * ptf * (bm25K + 1) / (ptf + bm25K)
		}
	}

	top := make([]memHit, 0, min(want, len(touched))+1)
	for _, di := range touched {
		d := &ix.docs[di]
		h := memHit{scoreKey(sc.acc[di]), d.created, d.id}
		if len(top) == want && !h.before(top[want-1]) {
			continue
		}
		// Sorted insertion (binary search for the slot).
		lo, hi := 0, len(top)
		for lo < hi {
			mid := (lo + hi) / 2
			if top[mid].before(h) {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		if len(top) < want {
			top = append(top, memHit{})
		}
		copy(top[lo+1:], top[lo:len(top)-1])
		top[lo] = h
	}
	sc.touched = touched[:0]
	ix.pool.Put(sc)
	ids := make([]int64, len(top))
	for i, h := range top {
		ids[i] = h.id
	}
	return ids
}

func containsInt32(s []int32, v int32) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// buildMemFromDB scans the table once, analyzing batches on all cores, and merges them in
// scan order into a fresh index.
func buildMemFromDB(ctx context.Context, db *sql.DB, k *searchKind) (*memIndex, error) {
	rows, err := db.QueryContext(ctx, "SELECT "+k.selectCols()+" FROM "+k.table)
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", k.table, err)
	}
	defer rows.Close()

	const batchSize = 256
	type batch struct {
		seq  int
		docs []rawDoc
	}
	type result struct {
		seq  int
		docs []*analyzedDoc
	}
	workers := runtime.GOMAXPROCS(0)
	in := make(chan batch, workers*2)
	out := make(chan result, workers*2)
	readErr := make(chan error, 1)

	go func() {
		defer close(in)
		seq := 0
		cur := make([]rawDoc, 0, batchSize)
		flush := func() bool {
			if len(cur) == 0 {
				return true
			}
			select {
			case in <- batch{seq, cur}:
			case <-ctx.Done():
				return false
			}
			seq++
			cur = make([]rawDoc, 0, batchSize)
			return true
		}
		var d rawDoc
		dests := d.dests(k)
		for rows.Next() {
			if err := rows.Scan(dests...); err != nil {
				readErr <- err
				return
			}
			cur = append(cur, d)
			if len(cur) == batchSize && !flush() {
				readErr <- ctx.Err()
				return
			}
		}
		if err := rows.Err(); err != nil {
			readErr <- err
			return
		}
		if !flush() {
			readErr <- ctx.Err()
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a := newMemdAnalyzer()
			for b := range in {
				r := result{seq: b.seq, docs: make([]*analyzedDoc, len(b.docs))}
				for i := range b.docs {
					r.docs[i] = analyzeDoc(a, k, &b.docs[i])
				}
				out <- r
			}
		}()
	}
	go func() { wg.Wait(); close(out) }()

	ix := newMemIndex(k)
	pending := map[int]result{}
	next := 0
	for r := range out {
		pending[r.seq] = r
		for {
			p, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			next++
			for _, d := range p.docs {
				ix.add(d)
			}
		}
	}
	select {
	case err := <-readErr:
		return nil, fmt.Errorf("scan %s: %w", k.table, err)
	default:
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ix.compact()
	return ix, nil
}
