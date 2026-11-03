package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"
)

// The SQL path: postings tables in DuckDB, scored by one aggregate query.
//
//	<p>_docs  one row per indexed row: id, project, typ, created_at, field lengths l1..l6
//	<p>_dict  term -> termid
//	<p>_post  (termid, id, tfp): tfp packs the per-field term frequencies, 8 bits per field
//
// <p>_post is written in termid order at backfill, so a query's termid IN (...) predicate
// prunes row groups by their min/max.

const memdSearchSchema = `
CREATE TABLE IF NOT EXISTS ox_docs (id BIGINT NOT NULL, project VARCHAR NOT NULL, typ VARCHAR NOT NULL, created_at TIMESTAMP NOT NULL,
	l1 INTEGER NOT NULL, l2 INTEGER NOT NULL, l3 INTEGER NOT NULL, l4 INTEGER NOT NULL, l5 INTEGER NOT NULL, l6 INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS ox_dict (termid INTEGER NOT NULL, term VARCHAR NOT NULL);
CREATE TABLE IF NOT EXISTS ox_post (termid INTEGER NOT NULL, id BIGINT NOT NULL, tfp BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS sx_docs (id BIGINT NOT NULL, project VARCHAR NOT NULL, typ VARCHAR NOT NULL, created_at TIMESTAMP NOT NULL,
	l1 INTEGER NOT NULL, l2 INTEGER NOT NULL, l3 INTEGER NOT NULL, l4 INTEGER NOT NULL, l5 INTEGER NOT NULL, l6 INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS sx_dict (termid INTEGER NOT NULL, term VARCHAR NOT NULL);
CREATE TABLE IF NOT EXISTS sx_post (termid INTEGER NOT NULL, id BIGINT NOT NULL, tfp BIGINT NOT NULL);
CREATE INDEX IF NOT EXISTS ox_dict_term ON ox_dict (term);
CREATE INDEX IF NOT EXISTS sx_dict_term ON sx_dict (term);
`

func packTF(tf [maxFields]uint8) int64 {
	var v int64
	for f, n := range tf {
		v |= int64(n) << (8 * f)
	}
	return v
}

// dbl renders x as a DuckDB DOUBLE literal.
func dbl(x float64) string { return "CAST(" + strconv.FormatFloat(x, 'g', -1, 64) + " AS DOUBLE)" }

// termIDs resolves analyzed query terms to dictionary ids (unknown terms are dropped).
func (s *memdSearcher) termIDs(k *searchKind, terms []string) ([]string, error) {
	ph := strings.TrimSuffix(strings.Repeat("?,", len(terms)), ",")
	args := make([]any, len(terms))
	for i, t := range terms {
		args[i] = t
	}
	rows, err := s.db.Query(fmt.Sprintf(`SELECT termid FROM %s_dict WHERE term IN (%s)`, k.prefix, ph), args...)
	if err != nil {
		return nil, fmt.Errorf("search terms: %w", err)
	}
	defer rows.Close()
	var tids []string
	for rows.Next() {
		var id int32
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		tids = append(tids, strconv.Itoa(int(id)))
	}
	return tids, rows.Err()
}

// docFilter renders f as a WHERE clause over the docs table aliased d ("" for no filter).
func docFilter(k *searchKind, f qFilter) (string, []any) {
	var conds []string
	var args []any
	if f.project != "" {
		conds = append(conds, "d.project = ?")
		args = append(args, f.project)
	}
	if k.typed && len(f.types) > 0 {
		conds = append(conds, "d.typ IN ("+strings.TrimSuffix(strings.Repeat("?,", len(f.types)), ",")+")")
		for _, t := range f.types {
			args = append(args, t)
		}
	}
	if f.start != nil {
		conds = append(conds, "d.created_at >= CAST(? AS TIMESTAMP)")
		args = append(args, memdTS(*f.start))
	}
	if f.end != nil {
		conds = append(conds, "d.created_at <= CAST(? AS TIMESTAMP)")
		args = append(args, memdTS(*f.end))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(conds, " AND "), args
}

// countSQL counts the documents matching any term, without scoring them.
func (s *memdSearcher) countSQL(k *searchKind, terms []string, f qFilter) (int, error) {
	tids, err := s.termIDs(k, terms)
	if err != nil || len(tids) == 0 {
		return 0, err
	}
	where, args := docFilter(k, f)
	var q string
	if where == "" {
		q = fmt.Sprintf(`SELECT count(*) FROM (SELECT DISTINCT id FROM %s_post WHERE termid IN (%s))`, k.prefix, strings.Join(tids, ","))
	} else {
		q = fmt.Sprintf(`SELECT count(*) FROM (SELECT DISTINCT p.id FROM %[1]s_post p JOIN %[1]s_docs d ON d.id = p.id %[3]s AND p.termid IN (%[2]s))`,
			k.prefix, strings.Join(tids, ","), where)
	}
	var n int
	if err := s.db.QueryRow(q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("search count %s: %w", k.table, err)
	}
	return n, nil
}

// rankSQL scores terms with a direct BM25F aggregate over the postings tables and returns the
// ids of the best want documents.
func (s *memdSearcher) rankSQL(k *searchKind, terms []string, f qFilter, want int) ([]int64, error) {
	tids, err := s.termIDs(k, terms)
	if err != nil || len(tids) == 0 {
		return nil, err
	}

	nf := len(k.cols)
	var avgs, pt []string
	for i := 0; i < nf; i++ {
		avgs = append(avgs, fmt.Sprintf("CASE WHEN avg(l%[1]d) = 0 THEN 1 ELSE avg(l%[1]d) END AS a%[1]d", i+1))
		pt = append(pt, fmt.Sprintf("%s * CAST((p.tfp >> %d) & 255 AS DOUBLE) / (%s + %s * d.l%d / st.a%d)",
			dbl(k.w[i]), 8*i, dbl(1-bm25B), dbl(bm25B), i+1, i+1))
	}
	where, fargs := docFilter(k, f)
	// sc scores each candidate once (grouped by id alone); the final select joins docs again
	// only for created_at, the second tie-break, over the rows whose rounded score reaches the
	// want-th best score.
	q := fmt.Sprintf(`
WITH p AS (SELECT termid, id, tfp FROM %[1]s_post WHERE termid IN (%[2]s)),
st AS (SELECT CAST(count(*) AS DOUBLE) AS n, %[3]s FROM %[1]s_docs),
df AS (SELECT p.termid, ln(1 + (st.n - p.c + 0.5) / (p.c + 0.5)) AS idf
	FROM (SELECT termid, CAST(count(*) AS DOUBLE) AS c FROM p GROUP BY termid) p CROSS JOIN st),
sc AS (
	SELECT x.id, sum(df.idf * x.pt * %[6]s / (x.pt + %[7]s)) AS s
	FROM (SELECT p.id, p.termid, %[4]s AS pt
		FROM p JOIN %[1]s_docs d ON d.id = p.id CROSS JOIN st %[5]s) x
	JOIN df ON df.termid = x.termid
	GROUP BY x.id
),
keyed AS (SELECT id, CAST(round(s * 1e9) AS BIGINT) AS k FROM sc)
SELECT keyed.id FROM keyed JOIN %[1]s_docs d ON d.id = keyed.id
WHERE keyed.k >= coalesce((SELECT k FROM keyed ORDER BY k DESC LIMIT 1 OFFSET ?), -9223372036854775807)
ORDER BY keyed.k DESC, d.created_at DESC, keyed.id DESC LIMIT ?`,
		k.prefix, strings.Join(tids, ","), strings.Join(avgs, ", "), strings.Join(pt, " + "), where,
		dbl(bm25K+1), dbl(bm25K))
	fargs = append(fargs, want-1, want)
	if memdSQLTrace != nil {
		memdSQLTrace(q, fargs)
	}
	rows, err := s.db.Query(q, fargs...)
	if err != nil {
		return nil, fmt.Errorf("search %s: %w", k.table, err)
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

// memdSQLTrace is a test hook that sees each scoring query before it runs.
var memdSQLTrace func(q string, args []any)

const sqlInsertChunk = 300

// writeDocSQL adds one document to the postings tables in one transaction, unless it is
// already there.
func (s *memdSearcher) writeDocSQL(k *searchKind, d *analyzedDoc) error {
	ks := s.st[k]
	ks.wmu.Lock()
	defer ks.wmu.Unlock()
	var exists int
	if err := s.db.QueryRow(fmt.Sprintf(`SELECT count(*) FROM %s_docs WHERE id = ?`, k.prefix), d.id).Scan(&exists); err != nil {
		return fmt.Errorf("search index: %w", err)
	}
	if exists > 0 {
		return nil
	}
	if !ks.haveNext {
		if err := s.db.QueryRow(fmt.Sprintf(`SELECT coalesce(max(termid) + 1, 0) FROM %s_dict`, k.prefix)).Scan(&ks.nextTerm); err != nil {
			return fmt.Errorf("search index: %w", err)
		}
		ks.haveNext = true
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("search index: %w", err)
	}
	defer tx.Rollback()

	// Resolve term ids, creating the missing terms.
	tid := make(map[string]int32, len(d.terms))
	for lo := 0; lo < len(d.terms); lo += sqlInsertChunk {
		hi := min(lo+sqlInsertChunk, len(d.terms))
		args := make([]any, 0, hi-lo)
		for _, t := range d.terms[lo:hi] {
			args = append(args, t.term)
		}
		rows, err := tx.Query(fmt.Sprintf(`SELECT term, termid FROM %s_dict WHERE term IN (%s)`, k.prefix,
			strings.TrimSuffix(strings.Repeat("?,", hi-lo), ",")), args...)
		if err != nil {
			return fmt.Errorf("search index: %w", err)
		}
		for rows.Next() {
			var t string
			var id int32
			if err := rows.Scan(&t, &id); err != nil {
				rows.Close()
				return err
			}
			tid[t] = id
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	next := ks.nextTerm
	var newTerms []docTerm
	for _, t := range d.terms {
		if _, ok := tid[t.term]; !ok {
			tid[t.term] = next
			next++
			newTerms = append(newTerms, t)
		}
	}
	if err := multiInsert(tx, k.prefix+"_dict", 2, len(newTerms), func(i int) []any {
		return []any{tid[newTerms[i].term], newTerms[i].term}
	}); err != nil {
		return err
	}
	if err := multiInsert(tx, k.prefix+"_post", 3, len(d.terms), func(i int) []any {
		return []any{tid[d.terms[i].term], d.id, packTF(d.terms[i].tf)}
	}); err != nil {
		return err
	}
	l := d.lens
	if _, err := tx.Exec(fmt.Sprintf(`INSERT INTO %s_docs VALUES (?, ?, ?, CAST(? AS TIMESTAMP), ?, ?, ?, ?, ?, ?)`, k.prefix),
		d.id, d.project, d.typ, memdTS(time.UnixMicro(d.created)), l[0], l[1], l[2], l[3], l[4], l[5]); err != nil {
		return fmt.Errorf("search index docs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("search index commit: %w", err)
	}
	ks.nextTerm = next
	return nil
}

func multiInsert(tx *sql.Tx, table string, width, n int, row func(i int) []any) error {
	for lo := 0; lo < n; lo += sqlInsertChunk {
		hi := min(lo+sqlInsertChunk, n)
		one := "(" + strings.TrimSuffix(strings.Repeat("?,", width), ",") + ")"
		var args []any
		for i := lo; i < hi; i++ {
			args = append(args, row(i)...)
		}
		q := "INSERT INTO " + table + " VALUES " + strings.TrimSuffix(strings.Repeat(one+",", hi-lo), ",")
		if _, err := tx.Exec(q, args...); err != nil {
			return fmt.Errorf("search index %s: %w", table, err)
		}
	}
	return nil
}

// backfill brings k's postings tables up to date on open: in bulk when they are empty, else
// by indexing only the missing rows.
func (s *memdSearcher) backfill(ctx context.Context, k *searchKind) error {
	var nSrc, nIdx int64
	if err := s.db.QueryRow(fmt.Sprintf(`SELECT (SELECT count(*) FROM %s), (SELECT count(*) FROM %s_docs)`, k.table, k.prefix)).Scan(&nSrc, &nIdx); err != nil {
		return err
	}
	if nSrc == nIdx {
		return nil
	}
	return s.indexMissing(ctx, k)
}

// backfillFull rebuilds k's postings tables from the table: analyze every row, then write the
// dictionary, postings (in termid order) and docs with appenders. The docs table is written
// last, so an interrupted run is redone from scratch.
func (s *memdSearcher) backfillFull(ctx context.Context, k *searchKind) error {
	t0 := time.Now()
	ks := s.st[k]
	ks.wmu.Lock()
	defer ks.wmu.Unlock()
	ix, err := buildMemFromDB(ctx, s.db, k)
	if err != nil {
		return err
	}
	for _, t := range []string{"_docs", "_dict", "_post"} {
		if _, err := s.db.ExecContext(ctx, "DELETE FROM "+k.prefix+t); err != nil {
			return fmt.Errorf("search backfill: %w", err)
		}
	}
	terms := make([]string, len(ix.post))
	for t, id := range ix.vocab {
		terms[id] = t
	}
	err = appendRows(ctx, s.db, k.prefix+"_dict", func(app *duckdb.Appender) error {
		for id, t := range terms {
			if err := app.AppendRow(int32(id), t); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("search backfill dict: %w", err)
	}
	err = appendRows(ctx, s.db, k.prefix+"_post", func(app *duckdb.Appender) error {
		for id, pl := range ix.post {
			for i := range pl {
				if err := app.AppendRow(int32(id), ix.docs[pl[i].doc].id, packTF(pl[i].tf)); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("search backfill postings: %w", err)
	}
	strs := make([]string, len(ix.strs))
	for str, id := range ix.strs {
		strs[id] = str
	}
	err = appendRows(ctx, s.db, k.prefix+"_docs", func(app *duckdb.Appender) error {
		for i := range ix.docs {
			d := &ix.docs[i]
			l := d.lens
			if err := app.AppendRow(d.id, strs[d.project], strs[d.typ], time.UnixMicro(d.created).UTC(),
				l[0], l[1], l[2], l[3], l[4], l[5]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("search backfill docs: %w", err)
	}
	ks.haveNext = false
	log.Printf("memory search: indexed %d %s rows (%d terms) in %v", len(ix.docs), k.table, len(terms), time.Since(t0).Round(time.Millisecond))
	// An in-memory index built before this no longer matches the tables.
	if ks.mem.Load() != nil {
		ks.mem.Store(nil)
		go func() {
			if err := s.buildMem(context.Background(), k); err != nil {
				log.Printf("memory search: rebuild %s: %v", k.name, err)
			}
		}()
	}
	return nil
}

func appendRows(ctx context.Context, db *sql.DB, table string, fill func(app *duckdb.Appender) error) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
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
