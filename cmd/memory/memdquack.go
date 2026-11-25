package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// memdQuackURI is where quack_serve listens: loopback only. Tests point it at a free port.
var memdQuackURI = "quack:127.0.0.1:9494"

// startQuack serves db over Quack on 127.0.0.1 so a DuckDB client can
// ATTACH 'quack:localhost:9494' AS q (TOKEN '<contents of dir/quack.token>').
// Failure is returned (and logged) for the caller to ignore: memory keeps running without Quack.
func startQuack(db *sql.DB, dir string) (err error) {
	defer func() {
		if err != nil {
			workerLogf("memory quack: not serving: %v", err)
		}
	}()
	// httpfs first: quack_serve needs its writable crypto module.
	for _, s := range []string{
		"SET autoinstall_known_extensions=false",
		"SET autoload_known_extensions=false",
		"LOAD " + sqlQuote(filepath.Join(memdExtDir(), "httpfs.duckdb_extension")),
		"LOAD " + sqlQuote(filepath.Join(memdExtDir(), "quack.duckdb_extension")),
	} {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("quack setup %q: %w", s, err)
		}
	}
	if err := checkQuackPortFree(memdQuackURI); err != nil {
		return err
	}
	token, err := quackToken(dir)
	if err != nil {
		return err
	}
	if err := installQuackReadOnly(db); err != nil {
		return err
	}
	var uri, url, echoed string
	if err := db.QueryRow("SELECT * FROM quack_serve("+sqlQuote(memdQuackURI)+", token="+sqlQuote(token)+")").Scan(&uri, &url, &echoed); err != nil {
		return fmt.Errorf("quack_serve %s: %w", memdQuackURI, err)
	}
	workerLogf("memory quack: serving %s (token in %s)", uri, filepath.Join(dir, "quack.token"))
	return nil
}

// checkQuackPortFree fails unless nothing listens on the Quack address. The quack extension
// binds with SO_REUSEPORT (native code, so Go cannot change it), which lets a second daemon share
// the port silently and split the first one's connections. A connect probe finds any listener
// regardless of its socket options, while connections lingering from a stopped server (TIME_WAIT,
// FIN_WAIT2) do not answer it, so an in-place restart still works. A bind probe would refuse that
// restart: the old connections' sockets lack SO_REUSEADDR. Only ECONNREFUSED counts as free; a
// timeout or any other error means something is there. A probe connection is opened and closed,
// which a live daemon sees as one empty client. The window before quack_serve binds is accepted.
func checkQuackPortFree(uri string) error {
	addr := strings.TrimPrefix(uri, "quack:")
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err == nil {
		c.Close()
		return fmt.Errorf("port %s already in use, not starting quack", addr)
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return nil
	}
	return fmt.Errorf("port %s already in use or unreachable, not starting quack: %w", addr, err)
}

// quackToken returns the token stored in dir/quack.token, creating the file with mode 0600
// (never chmod-ed afterwards) when it is missing or unusable.
func quackToken(dir string) (string, error) {
	path := filepath.Join(dir, "quack.token")
	if st, err := os.Stat(path); err == nil && st.Mode().Perm() == 0o600 {
		if b, err := os.ReadFile(path); err == nil {
			if tok := strings.TrimSpace(string(b)); len(tok) >= 32 {
				return tok, nil
			}
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("quack token: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("quack token dir: %w", err)
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("quack token: %w", err)
	}
	tok := hex.EncodeToString(raw[:])
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("quack token: %w", err)
	}
	if _, err := f.WriteString(tok + "\n"); err != nil {
		f.Close()
		return "", fmt.Errorf("quack token: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("quack token: %w", err)
	}
	return tok, nil
}

// quackReadOnlyMacro is the Quack authorization callback. quack_authorization_function names
// a macro (connection id, SQL text) -> VARCHAR that Quack runs for every statement a client
// sends; it returns the SQL to run (quack_nop_authorization returns its input) or raises an
// error to reject the statement. Quack has no read-only serve option or token scope
// (checked 2026-10-28: quack_serve takes only listen_uri, TLS, token and secret arguments),
// so this is an allow-list: the text must start with SELECT/WITH/FROM/DESCRIBE/SHOW/
// SUMMARIZE/EXPLAIN, or be a bare BEGIN/COMMIT/ROLLBACK (the client wraps reads in them), and
// must not mention a write, DDL, settings, file or extension keyword anywhere. A string
// literal containing one is refused too, which errs on the safe side. memory's own connections
// never pass through this callback.
const quackReadOnlyMacro = `CREATE OR REPLACE MACRO memory_quack_authz(conn, q) AS
CASE WHEN regexp_matches(lower(q), '^\s*(begin( transaction)?( read only)?|commit|rollback|abort|end)\s*;?\s*$')
  OR (regexp_matches(lower(q), '^\s*(select|with|from|describe|show|summarize|explain)\b')
  AND NOT regexp_matches(lower(q), '\b(insert|update|delete|create|drop|alter|copy|attach|detach|pragma|call|set|reset|install|load|export|import|merge|truncate|vacuum|checkpoint|use|grant|revoke|glob|getenv|quack_[a-z_]*|read_[a-z_]*|write_[a-z_]*|sniff_csv|parquet_[a-z_]*)\b'))
THEN q ELSE error('memory quack is read-only: statement refused') END`

func installQuackReadOnly(db *sql.DB) error {
	if err := withWrite(func() error {
		_, err := db.Exec(quackReadOnlyMacro)
		return err
	}); err != nil {
		return fmt.Errorf("quack read-only macro: %w", err)
	}
	if _, err := db.Exec("SET quack_authorization_function = 'memory_quack_authz'"); err != nil {
		return fmt.Errorf("quack authorization function: %w", err)
	}
	return nil
}
