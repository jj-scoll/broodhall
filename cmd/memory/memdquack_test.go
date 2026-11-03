package main

import (
	"database/sql"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// freeQuackURI points memdQuackURI at a free loopback port for the test.
func freeQuackURI(t *testing.T) (uri string, port int) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback networking: %v", err)
	}
	port = l.Addr().(*net.TCPAddr).Port
	l.Close()
	uri = fmt.Sprintf("quack:127.0.0.1:%d", port)
	old := memdQuackURI
	memdQuackURI = uri
	t.Cleanup(func() { memdQuackURI = old })
	return uri, port
}

// quackClient is a second, separate in-process DuckDB with the same extensions, as in the
// Task 1 spike (the Nix duckdb CLI cannot load the release extensions yet).
func quackClient(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	for _, s := range []string{
		"SET autoinstall_known_extensions=false",
		"SET autoload_known_extensions=false",
		"LOAD " + sqlQuote(filepath.Join(memdExtDir(), "httpfs.duckdb_extension")),
		"LOAD " + sqlQuote(filepath.Join(memdExtDir(), "quack.duckdb_extension")),
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return db
}

func countOf(t *testing.T, db *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestMemdQuack(t *testing.T) {
	db, dir := openTestMemd(t)
	_, port := freeQuackURI(t)
	if _, err := db.Exec(`INSERT INTO observations (title, source) VALUES ('seed', 'live')`); err != nil {
		t.Fatal(err)
	}
	if err := startQuack(db, dir); err != nil {
		t.Fatalf("startQuack: %v", err)
	}
	t.Cleanup(func() { db.Exec("SELECT * FROM quack_stop('" + memdQuackURI + "')") })

	tokenPath := filepath.Join(dir, "quack.token")
	st, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("token mode = %v, want 0600", st.Mode().Perm())
	}
	raw, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(raw))
	if len(token) < 32 {
		t.Fatalf("token too short: %d chars", len(token))
	}

	client := quackClient(t)
	if _, err := client.Exec(fmt.Sprintf("ATTACH 'quack:localhost:%d' AS q (TOKEN %s)", port, sqlQuote(token))); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if got := countOf(t, client, "SELECT count(*) FROM q.observations"); got != 1 {
		t.Fatalf("client sees %d observations, want 1", got)
	}
	// Joins, CTEs and aggregates are reads and must keep working.
	if got := countOf(t, client, "WITH o AS (SELECT * FROM q.observations) SELECT count(*) FROM o"); got != 1 {
		t.Fatalf("CTE read saw %d, want 1", got)
	}

	// Read-only: every write shape must fail and leave the server unchanged.
	writes := []string{
		"INSERT INTO q.observations (title, source) VALUES ('from client', 'live')",
		"UPDATE q.observations SET title = 'changed'",
		"DELETE FROM q.observations",
		"CREATE TABLE q.evil (x INT)",
		"DROP TABLE q.observations",
		"INSERT INTO q.observations SELECT * FROM q.observations",
		"CALL q.query('INSERT INTO observations (title, source) VALUES (''via query'', ''live'')')",
		"SELECT * FROM q.query('DELETE FROM observations')",
		"SELECT * FROM q.query('INSERT INTO observations (title, source) VALUES (''sneak'', ''live'')')",
		"SELECT * FROM q.query('/* hi */ WITH x AS (SELECT 1) INSERT INTO observations (title, source) VALUES (''cte'', ''live'')')",
		"SELECT * FROM q.query('COPY observations TO ''/tmp/x.csv''')",
	}
	for _, w := range writes {
		_, err := client.Exec(w)
		if err == nil {
			t.Errorf("write accepted: %s", w)
		} else if !strings.Contains(err.Error(), "read-only") {
			t.Errorf("%s: failed, but not by the read-only callback: %v", w, err)
		}
	}
	if got := countOf(t, db, "SELECT count(*) FROM observations"); got != 1 {
		t.Errorf("server has %d observations after client writes, want 1", got)
	}
	if got := countOf(t, db, "SELECT count(*) FROM observations WHERE title = 'seed'"); got != 1 {
		t.Errorf("seed row was modified")
	}
	if got := countOf(t, client, "SELECT count(*) FROM q.observations"); got != 1 {
		t.Errorf("client count after rejected writes = %d, want 1", got)
	}

	// The server's own writes are not affected by the authorization callback.
	if err := withWrite(func() error {
		_, err := db.Exec(`INSERT INTO observations (title, source) VALUES ('server', 'live')`)
		return err
	}); err != nil {
		t.Fatalf("server write after startQuack: %v", err)
	}
	if got := countOf(t, client, "SELECT count(*) FROM q.observations"); got != 2 {
		t.Errorf("client sees %d after server write, want 2", got)
	}

	// A wrong token is refused.
	bad := quackClient(t)
	if _, err := bad.Exec(fmt.Sprintf("ATTACH 'quack:localhost:%d' AS q (TOKEN 'not-the-token-at-all')", port)); err == nil {
		t.Error("attach with wrong token succeeded")
	}
}

func TestMemdQuackLoopbackOnly(t *testing.T) {
	db, dir := openTestMemd(t)
	_, port := freeQuackURI(t)
	if err := startQuack(db, dir); err != nil {
		t.Fatalf("startQuack: %v", err)
	}
	t.Cleanup(func() { db.Exec("SELECT * FROM quack_stop('" + memdQuackURI + "')") })
	data, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		t.Skip("no /proc/net/tcp")
	}
	// 0100007F is 127.0.0.1 in /proc/net/tcp; state 0A is LISTEN.
	want := fmt.Sprintf("0100007F:%04X", port)
	any := fmt.Sprintf("00000000:%04X", port)
	var loop bool
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[3] != "0A" {
			continue
		}
		if f[1] == any {
			t.Fatalf("quack listens on all interfaces: %s", line)
		}
		if f[1] == want {
			loop = true
		}
	}
	if !loop {
		t.Errorf("no loopback listener on port %d", port)
	}
}

func TestMemdQuackTokenReusedAndFailureReturnsError(t *testing.T) {
	db, dir := openTestMemd(t)
	freeQuackURI(t)
	if err := startQuack(db, dir); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(filepath.Join(dir, "quack.token"))
	db.Exec("SELECT * FROM quack_stop('" + memdQuackURI + "')")
	if err := startQuack(db, dir); err != nil {
		t.Fatalf("restart: %v", err)
	}
	t.Cleanup(func() { db.Exec("SELECT * FROM quack_stop('" + memdQuackURI + "')") })
	second, _ := os.ReadFile(filepath.Join(dir, "quack.token"))
	if string(first) != string(second) {
		t.Error("token changed across restart")
	}

	t.Setenv("MEMORY_EXT_DIR", t.TempDir())
	db2, dir2 := openTestMemdNoFts(t)
	if err := startQuack(db2, dir2); err == nil {
		t.Error("startQuack with missing extensions returned nil")
	}
	if _, err := os.Stat(filepath.Join(dir2, "quack.token")); err == nil {
		t.Error("token file written although startup failed before serving")
	}
}

// openTestMemdNoFts opens a plain DuckDB (no fts) so a missing MEMORY_EXT_DIR can be tested.
func openTestMemdNoFts(t *testing.T) (*sql.DB, string) {
	t.Helper()
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, t.TempDir()
}
