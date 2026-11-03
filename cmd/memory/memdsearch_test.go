package main

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// termsOf lists the index terms the analyzer emits for text, in order.
func termsOf(text string) []string {
	var out []string
	for _, tk := range newMemdAnalyzer().analyze(text) {
		out = append(out, tk.term)
	}
	return out
}

func hasTerms(got []string, want ...string) bool {
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

// One case per miss category from the quality report (not per query wording).
func TestMemdAnalyzerCategories(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []string // terms that must be emitted
		exclude []string // terms that must not be emitted
	}{
		{"camelCase splits into parts and keeps the whole", "lazyPage", []string{memdStem("lazypage"), "page"}, nil},
		{"PascalCase compound", "ChangeSetTray.test.tsx", []string{"changesettray.test.tsx", memdStem("tray"), "test", "tsx", "set"}, nil},
		{"acronym boundary", "HTTPServer", []string{memdStem("httpserver"), "http", memdStem("server")}, nil},
		{"hyphenated name still splits", "definity-rvtest-psaSchedules", []string{memdStem("definity"), memdStem("rvtest"), memdStem("psaschedules"), memdStem("schedules")}, nil},
		{"dotted identifier", "store.duckdb.pre", []string{"store.duckdb.pre", "store", memdStem("duckdb"), "pre"}, nil},
		{"underscored identifier", "aws_autoscaling_group.this", []string{"aws_autoscaling_group.this", "aws", memdStem("autoscaling"), memdStem("group"), memdStem("this")}, nil},
		{"letter-digit identifiers are not split", "cre8 sha256 v2", []string{"cre8", "sha256", "v2"}, []string{"cre", "sha", "8", "256"}},
		{"short and numeric parts are dropped", "a_1_b.42.x", nil, []string{"a", "b", "1", "42", "x"}},
		{"stemming joins inflections", "rejection rejected rejects", []string{"reject"}, []string{"rejection", "rejected", "rejects"}},
		{"plural meets singular", "clients partners", []string{"client", "partner"}, []string{"clients", "partners"}},
		{"trailing dot is trimmed", "it did not pass. Next", []string{memdStem("pass"), "next"}, []string{"pass."}},
		{"leading and trailing underscores are trimmed", "_private_ and __dunder__", []string{memdStem("private"), memdStem("dunder")}, []string{"_private_", "__dunder__"}},
		{"identifiers with dots are never stemmed", "parsers.go", []string{"parsers.go", memdStem("parsers"), "go"}, nil},
		{"non-ASCII separates words", "naïve café", []string{"na", "ve", "caf"}, []string{"naïve"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := termsOf(tt.in)
			if !hasTerms(got, tt.want...) {
				t.Errorf("terms(%q) = %v, missing some of %v", tt.in, got, tt.want)
			}
			for _, x := range tt.exclude {
				if hasTerms(got, x) {
					t.Errorf("terms(%q) = %v, must not contain %q", tt.in, got, x)
				}
			}
		})
	}
}

// Only whole words count toward field length; parts add term frequency but not length.
func TestMemdAnalyzerWholeFlag(t *testing.T) {
	toks := newMemdAnalyzer().analyze("lazyPage fix")
	wholes := 0
	for _, tk := range toks {
		if tk.whole {
			wholes++
		}
	}
	if wholes != 2 {
		t.Errorf("whole tokens = %d, want 2 (%v)", wholes, toks)
	}
}

func TestMemdStem(t *testing.T) {
	for in, want := range map[string]string{
		"rejection": "reject", "rejected": "reject", "organized": "organ", "organization": "organ",
		"partners": "partner", "clusters": "cluster", "warnings": "warn", "reducing": "reduc",
		"reduced": "reduc", "inconsistent": "inconsist", "inconsistency": "inconsist",
		"caresses": "caress", "ponies": "poni", "agreed": "agre", "plastered": "plaster",
		"motoring": "motor", "happy": "happi", "relational": "relat", "conditional": "condit",
		"hopeful": "hope", "goodness": "good", "allowance": "allow", "controll": "control",
		"moves": "move", "transactions": "transact", "repository": "repositori",
	} {
		if got := memdStem(in); got != want {
			t.Errorf("memdStem(%q) = %q, want %q", in, got, want)
		}
	}
}

// openSearchDB opens a fresh test DB and inserts obs rows directly in SQL (the catch-up
// path), returning the ids.
func openSearchDB(t *testing.T) *sql.DB {
	t.Helper()
	db, _ := openTestMemd(t)
	return db
}

func insObs(t *testing.T, db *sql.DB, project, typ, title, sub, narr string, ts time.Time) int64 {
	t.Helper()
	var id int64
	err := db.QueryRow(`INSERT INTO observations (project, session_id, type, title, subtitle, narrative, source, created_at)
		VALUES (?, 's', ?, ?, ?, ?, 'live', CAST(? AS TIMESTAMP)) RETURNING id`, project, typ, title, sub, narr, memdTS(ts)).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

var searchPaths = []string{"mem", "sql"}

// searchOn ranks obs ids for q on the named path ("mem" builds the in-memory index first).
func searchOn(t *testing.T, db *sql.DB, path, q string, f qFilter, n int) []int64 {
	t.Helper()
	s := searcherFor(db)
	if path == "mem" {
		if err := s.buildMem(context.Background(), obsKind); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := s.rank(obsKind, q, f, n, 0, path)
	if err != nil {
		t.Fatalf("%s rank %q: %v", path, q, err)
	}
	return ids
}

func TestMemdFieldWeights(t *testing.T) {
	for _, path := range searchPaths {
		t.Run(path, func(t *testing.T) {
			db := openSearchDB(t)
			ts := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
			// Same length profile; "gizmo" is in the title of one and only in the narrative of the
			// other. The body-only doc is the newer one, so a tie would favor it.
			titleDoc := insObs(t, db, "p", "bugfix", "gizmo repaired", "", "the widget was fixed", ts)
			bodyDoc := insObs(t, db, "p", "bugfix", "widget repaired", "", "the gizmo was fixed", ts.Add(time.Hour))
			got := searchOn(t, db, path, "gizmo", qFilter{}, 10)
			if len(got) != 2 || got[0] != titleDoc || got[1] != bodyDoc {
				t.Errorf("ranking = %v, want title match %d before body match %d", got, titleDoc, bodyDoc)
			}
		})
	}
}

// fixtureCorpus inserts n synthetic observations with identifiers, shared terms and a few projects.
func fixtureCorpus(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	rng := rand.New(rand.NewSource(7))
	vocab := strings.Fields(`parser lexer token cache widget export command store fuzz entry point nil guard lazyPage
		changeSetTray rejected rejection clients partners profile organized config store.duckdb aws_autoscaling_group.this
		deploy timeout lambda terraform glue schedules runs frontend label move moves tests green pass. failed`)
	projects := []string{"proj-a", "proj-b", "proj-c"}
	types := []string{"bugfix", "feature", "discovery"}
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	words := func(k int) string {
		w := make([]string, k)
		for i := range w {
			w[i] = vocab[rng.Intn(len(vocab))]
		}
		return strings.Join(w, " ")
	}
	for i := 0; i < n; i++ {
		insObs(t, db, projects[rng.Intn(3)], types[rng.Intn(3)], words(2+rng.Intn(5)), words(rng.Intn(4)),
			words(5+rng.Intn(30)), ts.Add(time.Duration(rng.Intn(40))*time.Hour))
	}
}

func TestMemdPathParity(t *testing.T) {
	db := openSearchDB(t)
	fixtureCorpus(t, db, 250)
	s := searcherFor(db)
	if err := s.buildMem(context.Background(), obsKind); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, q := range []string{"parser", "lazy page rejection", "ChangeSetTray tests green", "store.duckdb deploy", "aws autoscaling",
		"clients partners profile", "pass", "nonexistentterm", "frontend label moves", "the and of"} {
		for _, f := range []qFilter{{}, {project: "proj-a"}, {types: []string{"bugfix", "feature"}}, {start: &start}, {project: "proj-b", types: []string{"discovery"}, start: &start}} {
			mem, err := s.rank(obsKind, q, f, 25, 0, "mem")
			if err != nil {
				t.Fatal(err)
			}
			sq, err := s.rank(obsKind, q, f, 25, 0, "sql")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(mem, sq) {
				t.Errorf("query %q filter %+v:\n mem=%v\n sql=%v", q, f, mem, sq)
			}
			if q == "parser" && len(mem) == 0 {
				t.Errorf("query %q matched nothing; fixture is not exercising the paths", q)
			}
		}
	}
	// Offsets page through the same order on both paths.
	all, _ := s.rank(obsKind, "parser", qFilter{}, 30, 0, "sql")
	for _, path := range searchPaths {
		page, err := s.rank(obsKind, "parser", qFilter{}, 10, 5, path)
		if err != nil || !reflect.DeepEqual(page, all[5:15]) {
			t.Errorf("%s offset page = %v (err %v), want %v", path, page, err, all[5:15])
		}
	}
}

// A summary index parity check: same two paths over the summaries table.
func TestMemdSummaryParity(t *testing.T) {
	db := openSearchDB(t)
	for i := 0; i < 40; i++ {
		if _, err := db.Exec(`INSERT INTO summaries (project, session_id, request, learned, notes, source)
			VALUES ('p', 's', ?, ?, ?, 'live')`, fmt.Sprintf("Harden parser %d", i), strings.Repeat("token ", i%5), "lazyPage rejected"); err != nil {
			t.Fatal(err)
		}
	}
	s := searcherFor(db)
	if err := s.buildMem(context.Background(), sumKind); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"parser", "token rejection", "lazy page"} {
		mem, err1 := s.rank(sumKind, q, qFilter{}, 20, 0, "mem")
		sq, err2 := s.rank(sumKind, q, qFilter{}, 20, 0, "sql")
		if err1 != nil || err2 != nil || len(mem) == 0 || !reflect.DeepEqual(mem, sq) {
			t.Errorf("summaries %q: mem=%v (%v) sql=%v (%v)", q, mem, err1, sq, err2)
		}
	}
}

// A row written after the index was built is found by the next search, on both paths, whether
// it arrives through a write path (save_memory) or directly in SQL (caught up on the next search).
func TestMemdFreshness(t *testing.T) {
	db := openSearchDB(t)
	fixtureCorpus(t, db, 60)
	s := searcherFor(db)
	if err := s.buildMem(context.Background(), obsKind); err != nil {
		t.Fatal(err)
	}
	if _, err := memSaveMemory(db, map[string]any{"text": "zorblax frobnicator calibrated", "project": "proj-a"}); err != nil {
		t.Fatal(err)
	}
	direct := insObs(t, db, "proj-a", "bugfix", "quuxplex direct insert", "", "written straight into the table", time.Now())
	for _, path := range searchPaths {
		for q, wantNew := range map[string]bool{"zorblax": true, "quuxplex": true} {
			ids, err := s.rank(obsKind, q, qFilter{}, 5, 0, path)
			if err != nil || len(ids) != 1 {
				t.Errorf("%s search %q = %v (err %v), want exactly the new row", path, q, ids, err)
			}
			_ = wantNew
		}
		if ids, _ := s.rank(obsKind, "quuxplex", qFilter{}, 5, 0, path); len(ids) == 1 && ids[0] != direct {
			t.Errorf("%s: quuxplex found %v, want %d", path, ids, direct)
		}
	}
	// And through the tool entry point.
	out, err := memSearch(db, map[string]any{"query": "zorblax"})
	if err != nil || !strings.Contains(out, "frobnicator") && !strings.Contains(out, "zorblax") {
		t.Errorf("memSearch after save_memory = %q (err %v)", out, err)
	}
}

// While the in-memory index is still building, searches answer through SQL and do not wait.
func TestMemdSearchDuringBuild(t *testing.T) {
	db := openSearchDB(t)
	fixtureCorpus(t, db, 80)
	s := searcherFor(db)
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	memdIndexBuildHook = func() { once.Do(func() { close(entered) }); <-release }
	t.Cleanup(func() { memdIndexBuildHook = nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.buildAll(ctx) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("build never reached its hook")
	}
	res := make(chan string, 1)
	go func() {
		out, err := memSearch(db, map[string]any{"query": "parser", "limit": 5})
		if err != nil {
			out = "ERR " + err.Error()
		}
		res <- out
	}()
	select {
	case out := <-res:
		if !strings.Contains(out, "Found") {
			t.Errorf("search during build = %q", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("search blocked while the index was building")
	}
	if got := s.pathFor(obsKind); got != "sql" {
		t.Errorf("path during build = %q, want sql", got)
	}
	// A write during the build is not lost.
	if _, err := memSaveMemory(db, map[string]any{"text": "midbuild marker snorkelwhistle", "project": "proj-a"}); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("build did not finish")
	}
	if got := s.pathFor(obsKind); got != "mem" {
		t.Errorf("path after build = %q, want mem", got)
	}
	ids, err := s.rank(obsKind, "snorkelwhistle", qFilter{}, 5, 0, "mem")
	if err != nil || len(ids) != 1 {
		t.Errorf("mem search for the mid-build write = %v (err %v)", ids, err)
	}
}

// Rows that exist before the postings tables do (an upgrade) are backfilled on open.
func TestMemdBackfillOnMigrate(t *testing.T) {
	db := openSearchDB(t)
	fixtureCorpus(t, db, 50)
	for _, p := range []string{"ox", "sx"} {
		for _, tb := range []string{"_docs", "_dict", "_post"} {
			if _, err := db.Exec("DELETE FROM " + p + tb); err != nil {
				t.Fatal(err)
			}
		}
	}
	searchersReset(db)
	if err := memdMigrate(db); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM ox_docs`).Scan(&n); err != nil || n != 50 {
		t.Errorf("ox_docs after migrate = %d (err %v), want 50", n, err)
	}
	ids, err := searcherFor(db).rank(obsKind, "parser", qFilter{}, 5, 0, "sql")
	if err != nil || len(ids) == 0 {
		t.Errorf("sql search after backfill = %v (err %v)", ids, err)
	}
}

// countHits (used when only the number of hits is displayed) agrees with the ranking's length.
func TestMemdCountHits(t *testing.T) {
	db := openSearchDB(t)
	fixtureCorpus(t, db, 120)
	s := searcherFor(db)
	if err := s.buildMem(context.Background(), obsKind); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, q := range []string{"parser", "lazy page rejection", "nonexistentterm", "store.duckdb deploy"} {
		for _, f := range []qFilter{{}, {project: "proj-a"}, {types: []string{"bugfix"}}, {start: &start}} {
			for _, lo := range [][2]int{{100, 0}, {5, 0}, {10, 7}, {10, 500}} {
				want, err := s.rank(obsKind, q, f, lo[0], lo[1], "sql")
				if err != nil {
					t.Fatal(err)
				}
				for _, path := range searchPaths {
					got, err := s.countHitsOn(obsKind, q, f, lo[0], lo[1], path)
					if err != nil || got != len(want) {
						t.Errorf("%s countHits(%q, %+v, limit %d offset %d) = %d (err %v), want %d", path, q, f, lo[0], lo[1], got, err, len(want))
					}
				}
			}
		}
	}
}
