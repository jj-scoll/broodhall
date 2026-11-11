package main

import (
	"context"
	"database/sql"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// openTestMemd opens a fresh memory DB in a temp dir. MEMORY_EXT_DIR stays at the value
// TestMain fixed (testExtDir), where the signed fts extension lives.
func openTestMemd(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MEMORY_DIR", dir)
	db, err := openMemdDB(dir)
	if err != nil {
		t.Fatalf("openMemdDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, dir
}

func memdTables(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT table_name FROM information_schema.tables WHERE table_schema = 'main' ORDER BY table_name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	return names
}

func TestMigrateIdempotent(t *testing.T) {
	db, _ := openTestMemd(t)
	want := []string{"api_usage", "compactions", "deliverables", "import_watermarks", "loss_receipts", "observation_files", "observations",
		"observer_health", "project_aliases", "ox_dict", "ox_docs", "ox_post", "prompts", "queue", "sessions", "summaries", "summary_requests",
		"sx_dict", "sx_docs", "sx_post", "tool_uses", "transcript_offsets", "work_state"}
	sort.Strings(want)
	if got := memdTables(t, db); !reflect.DeepEqual(got, want) {
		t.Fatalf("tables after open:\n got %v\nwant %v", got, want)
	}
	for i := 0; i < 2; i++ {
		if err := memdMigrate(db); err != nil {
			t.Fatalf("memdMigrate call %d: %v", i+1, err)
		}
	}
	if got := memdTables(t, db); !reflect.DeepEqual(got, want) {
		t.Fatalf("tables after re-migrate:\n got %v\nwant %v", got, want)
	}
}

func TestMemdDir(t *testing.T) {
	t.Setenv("MEMORY_DIR", "/x/y")
	if got := memdDir(); got != "/x/y" {
		t.Errorf("memdDir() = %q, want /x/y", got)
	}
}

func insertObs(t *testing.T, db *sql.DB, title, narrative string) int64 {
	t.Helper()
	var id int64
	err := db.QueryRow(`INSERT INTO observations (project, session_id, type, title, narrative, source)
		VALUES ('p', 's', 'bugfix', ?, ?, 'live') RETURNING id`, title, narrative).Scan(&id)
	if err != nil {
		t.Fatalf("insert observation: %v", err)
	}
	return id
}

func TestFTSTokens(t *testing.T) {
	db, dir := openTestMemd(t)
	a := insertObs(t, db, "fix foo_bar.go ERR_CODE_42", "")
	b := insertObs(t, db, "Fix guard in cmd/claude-tools/pretooluse.go", "hook died with EPIPE, exit 255")
	if _, err := db.Exec(`INSERT INTO summaries (project, session_id, request, learned, source)
		VALUES ('p', 's', 'why does SIGPIPE_HANDLER leak', 'goroutine in memory.go', 'live')`); err != nil {
		t.Fatal(err)
	}

	check := func(t *testing.T, q string, want ...int64) {
		t.Helper()
		got, err := ftsSearch(db, q)
		if err != nil {
			t.Fatalf("ftsSearch(%q): %v", q, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ftsSearch(%q) = %v, want %v", q, got, want)
		}
	}
	runAll := func(t *testing.T) {
		tests := []struct {
			q    string
			want []int64
		}{
			{"ERR_CODE_42", []int64{a}},
			{"foo_bar.go", []int64{a}},
			{"pretooluse.go", []int64{b}},
			{"cmd/claude-tools/pretooluse.go", []int64{b}},
			{"EPIPE", []int64{b}},
			{"255", []int64{b}},
			{"nonexistentterm", nil},
		}
		for _, tt := range tests {
			t.Run(tt.q, func(t *testing.T) { check(t, tt.q, tt.want...) })
		}
		t.Run("summaries", func(t *testing.T) {
			var n int
			err := db.QueryRow(`SELECT count(*) FROM summaries WHERE fts_main_summaries.match_bm25(id, 'SIGPIPE_HANDLER') IS NOT NULL`).Scan(&n)
			if err != nil || n != 1 {
				t.Errorf("summaries hit count = %d, err %v; want 1", n, err)
			}
			err = db.QueryRow(`SELECT count(*) FROM summaries WHERE fts_main_summaries.match_bm25(id, 'memory.go') IS NOT NULL`).Scan(&n)
			if err != nil || n != 1 {
				t.Errorf("summaries learned-column hit count = %d, err %v; want 1", n, err)
			}
		})
	}

	t.Run("fresh", runAll)

	// A second migration on a DB that already has rows must leave search working,
	// including for rows inserted afterwards.
	if err := memdMigrate(db); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	t.Run("after re-migrate", runAll)
	c := insertObs(t, db, "added later zzlate.go", "")
	check(t, "zzlate.go", c)

	// And across close/reopen of the file DB.
	db.Close()
	db2, err := openMemdDB(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	db = db2
	t.Run("after reopen", runAll)
	d := insertObs(t, db, "after reopen yyreopen.go", "")
	check(t, "yyreopen.go", d)
}

func TestFTSDeleteInsert(t *testing.T) {
	db, _ := openTestMemd(t)
	id := insertObs(t, db, "old title oldname.go", "")
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`DELETE FROM observations WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO observations (id, project, session_id, type, title, source) VALUES (?, 'p', 's', 'bugfix', 'new title newname.go', 'live')`, id); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got, _ := ftsSearch(db, "newname.go"); !reflect.DeepEqual(got, []int64{id}) {
		t.Errorf("new title: got %v", got)
	}
	if got, _ := ftsSearch(db, "oldname.go"); len(got) != 0 {
		t.Errorf("old title still indexed: %v", got)
	}
}

// TestMemdSettingsPerConnection holds two pooled connections at once and checks that each
// has extension autoinstall/autoload off and a working fts index.
func TestMemdSettingsPerConnection(t *testing.T) {
	db, _ := openTestMemd(t)
	id := insertObs(t, db, "pooled zzpool.go", "")
	ctx := context.Background()
	c1, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	c2, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	for i, c := range []*sql.Conn{c1, c2} {
		for _, name := range []string{"autoinstall_known_extensions", "autoload_known_extensions"} {
			var v string
			if err := c.QueryRowContext(ctx, `SELECT current_setting('`+name+`')::VARCHAR`).Scan(&v); err != nil || v != "false" {
				t.Errorf("conn %d %s = %q, err %v; want false", i+1, name, v, err)
			}
		}
		var got int64
		err := c.QueryRowContext(ctx, `SELECT id FROM observations WHERE fts_main_observations.match_bm25(id, 'zzpool.go') IS NOT NULL`).Scan(&got)
		if err != nil || got != id {
			t.Errorf("conn %d fts: got %d, err %v; want %d", i+1, got, err, id)
		}
	}
}

// Columns are naive TIMESTAMPs holding UTC (memdTS). A DEFAULT current_timestamp must store UTC
// too, on every pooled connection, not the host's wall clock. Only fails on a non-UTC host.
func TestMemdDefaultTimestampsAreUTC(t *testing.T) {
	db, _ := openTestMemd(t)
	ctx := context.Background()
	for i := 1; i <= 2; i++ {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		var s string
		err = c.QueryRowContext(ctx, `INSERT INTO observations (project, session_id, type, title, source)
			VALUES ('p', 's', 'bugfix', 'tz', 'live') RETURNING CAST(created_at AS VARCHAR)`).Scan(&s)
		if err != nil {
			t.Fatal(err)
		}
		got, err := time.Parse("2006-01-02 15:04:05.999999", s)
		if err != nil {
			t.Fatal(err)
		}
		if d := time.Since(got); d < -time.Minute || d > time.Minute {
			t.Errorf("conn %d default created_at = %s, %v from UTC now; want UTC", i, s, d.Round(time.Minute))
		}
	}
}

func TestOpenMemdDBMissingExtension(t *testing.T) {
	t.Setenv("MEMORY_EXT_DIR", t.TempDir())
	_, err := openMemdDB(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "fts.duckdb_extension") {
		t.Errorf("err = %v, want one naming fts.duckdb_extension", err)
	}
}
