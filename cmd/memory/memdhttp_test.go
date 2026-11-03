package main

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// Fixtures are synthetic.

func httpDo(t *testing.T, h http.Handler, method, target, body string) (int, string, http.Header) {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String(), rec.Header()
}

func httpSeedObs(t *testing.T, db *sql.DB, id int, project, title, path, source string) {
	t.Helper()
	ex := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	ex(`INSERT INTO sessions (session_id, project) VALUES ('hs', ?) ON CONFLICT DO NOTHING`, project)
	ex(`INSERT INTO observations (id, project, session_id, type, title, subtitle, narrative, source, created_at)
		VALUES (?,?, 'hs', 'change', ?, 'sub', 'narr', ?, CAST('2026-03-02 09:00:00' AS TIMESTAMP) + to_minutes(?))`,
		id, project, title, source, id)
	ex(`INSERT INTO observation_files (observation_id, path, kind) VALUES (?,?,'modified')`, id, path)
}

func TestMemdHTTPSocketMode(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "m.sock")
	ln, err := listenSocket(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("socket mode = %o, want 600", got)
	}
	db, _ := openTestMemd(t)
	srv := &http.Server{Handler: memdHandler(db)}
	go srv.Serve(ln)
	defer srv.Close()
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	resp, err := c.Get("http://memory/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}
}

func TestMemdHTTPListenSocketStale(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "m.sock")
	// A stale socket: bound, then closed without unlinking.
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("stale socket not left behind: %v", err)
	}
	ln, err := listenSocket(sock)
	if err != nil {
		t.Fatalf("stale socket should be replaced: %v", err)
	}
	defer ln.Close()

	// A live socket must not be stolen.
	if ln2, err := listenSocket(sock); err == nil {
		ln2.Close()
		t.Fatal("second listenSocket on a live socket succeeded")
	}
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("live socket was removed: %v", err)
	}
	c.Close()
}

func TestMemdHTTPByFile(t *testing.T) {
	db, _ := openTestMemd(t)
	for i := 1; i <= 5; i++ {
		httpSeedObs(t, db, i, "proj-a", fmt.Sprintf("Change number %d", i), "src/widget/parser.go", "live")
	}
	httpSeedObs(t, db, 6, "proj-a", "Other file", "src/other.go", "live")
	httpSeedObs(t, db, 7, "proj-b", "Other project", "src/widget/parser.go", "imported")
	h := memdHandler(db)

	code, body, _ := httpDo(t, h, "GET", "/by-file?path=src/widget/parser.go&project=proj-a", "")
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	if n := strings.Count(body, "#"); n != 3 {
		t.Fatalf("got %d observations, want 3:\n%s", n, body)
	}
	if len(body) > 1500 {
		t.Fatalf("len = %d > 1500", len(body))
	}
	if !strings.Contains(body, "Change number 5") || strings.Contains(body, "Change number 2") || strings.Contains(body, "Other") {
		t.Fatalf("want newest three of proj-a only:\n%s", body)
	}

	// absolute path matches a stored relative suffix? no: the stored path is the suffix.
	_, body, _ = httpDo(t, h, "GET", "/by-file?path=/repo/src/widget/parser.go&project=proj-a", "")
	if body != "" {
		t.Fatalf("stored relative path should not match a longer query:\n%s", body)
	}
	// stored absolute path matches a relative query by '/'-suffix.
	httpSeedObs(t, db, 8, "proj-a", "Abs stored", "/repo/lib/util.go", "imported")
	_, body, _ = httpDo(t, h, "GET", "/by-file?path=lib/util.go&project=proj-a", "")
	if !strings.Contains(body, "Abs stored") {
		t.Fatalf("suffix match missing:\n%s", body)
	}
	// LIKE wildcards in the query are literal.
	_, body, _ = httpDo(t, h, "GET", "/by-file?path=%25&project=proj-a", "")
	if body != "" {
		t.Fatalf("wildcard matched:\n%s", body)
	}

	code, body, _ = httpDo(t, h, "GET", "/by-file?path=nope/none.go&project=proj-a", "")
	if code != 200 || body != "" {
		t.Fatalf("no match: code=%d body=%q", code, body)
	}
	if code, _, _ = httpDo(t, h, "GET", "/by-file?project=proj-a", ""); code != 400 {
		t.Fatalf("missing path = %d, want 400", code)
	}
}

func TestMemdHTTPByFileMultibyteTruncation(t *testing.T) {
	db, _ := openTestMemd(t)
	for i := 1; i <= 3; i++ {
		httpSeedObs(t, db, i, "proj-a", strings.Repeat("日本語é", 200), "src/a.go", "live")
	}
	_, body, _ := httpDo(t, memdHandler(db), "GET", "/by-file?path=src/a.go&project=proj-a", "")
	if len(body) > 1500 || len(body) < 1000 {
		t.Fatalf("len = %d, want truncated near 1500", len(body))
	}
	if !utf8.ValidString(body) {
		t.Fatal("truncated body is not valid UTF-8")
	}
}

func TestMemdHTTPContext(t *testing.T) {
	db, _ := openTestMemd(t)
	httpSeedObs(t, db, 1, "proj-a", "Something happened", "src/a.go", "live")
	h := memdHandler(db)
	code, body, hdr := httpDo(t, h, "GET", "/context?project=proj-a", "")
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(body, "Something happened") {
		t.Fatalf("observation missing: %s", body)
	}
	if len(body) > contextGuard {
		t.Fatalf("len %d over guard %d", len(body), contextGuard)
	}
	if !strings.HasPrefix(body, `{"hookSpecificOutput"`) || strings.HasSuffix(body, "\n\n") {
		t.Fatalf("not contextJSON bytes: %q", body[:min(60, len(body))])
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}
	if code, _, _ = httpDo(t, h, "GET", "/context", ""); code != 400 {
		t.Fatalf("missing project = %d, want 400", code)
	}
}

func TestMemdHTTPTools(t *testing.T) {
	db, _ := openTestMemd(t)
	httpSeedObs(t, db, 100, "proj-a", "Widget parser guard", "src/widget/parser.go", "live")
	h := memdHandler(db)

	code, body, _ := httpDo(t, h, "POST", "/tool/search", `{"query":"Widget","project":"proj-a"}`)
	if code != 200 || !strings.Contains(body, "Widget parser guard") {
		t.Fatalf("search: %d %s", code, body)
	}

	code, body, _ = httpDo(t, h, "POST", "/tool/save_memory", `{"text":"a durable note","project":"proj-a"}`)
	if code != 200 || !strings.Contains(body, "Saved memory") {
		t.Fatalf("save_memory: %d %s", code, body)
	}

	code, body, _ = httpDo(t, h, "POST", "/tool/session_start_context", `{"project":"proj-a"}`)
	if code != 200 || !strings.Contains(body, "Widget parser guard") {
		t.Fatalf("session_start_context: %d %s", code, body)
	}
	code, body, _ = httpDo(t, h, "POST", "/tool/session_start_context", `{"projects":"proj-x, proj-a"}`)
	if code != 200 || !strings.Contains(body, "Widget parser guard") {
		t.Fatalf("session_start_context projects chain uses last: %d %s", code, body)
	}
	if code, _, _ = httpDo(t, h, "POST", "/tool/session_start_context", `{}`); code != 400 {
		t.Fatalf("session_start_context without project = %d", code)
	}

	// Tool-level "Error:"/"Invalid" text (nil error) maps to 400 with the text as the body.
	code, body, _ = httpDo(t, h, "POST", "/tool/save_memory", `{"text":""}`)
	if code != 400 || !strings.HasPrefix(body, "Error:") {
		t.Fatalf("tool error text: %d %s", code, body)
	}
	code, body, _ = httpDo(t, h, "POST", "/tool/timeline", `{"anchor":"x1"}`)
	if code != 400 || !strings.HasPrefix(body, "Invalid") {
		t.Fatalf("invalid text: %d %s", code, body)
	}

	if code, _, _ = httpDo(t, h, "POST", "/tool/nope", `{}`); code != 404 {
		t.Fatalf("unknown tool = %d, want 404", code)
	}
	if code, _, _ = httpDo(t, h, "POST", "/tool/smart_search", `{}`); code != 404 {
		t.Fatalf("smart_search = %d, want 404", code)
	}
	if code, _, _ = httpDo(t, h, "POST", "/tool/search", `{bad`); code != 400 {
		t.Fatalf("bad json = %d, want 400", code)
	}
	if code, _, _ = httpDo(t, h, "GET", "/tool/search", ``); code != 405 {
		t.Fatalf("GET tool = %d, want 405", code)
	}
	// Empty body means no arguments (the handler still runs; it is not a 404 or a JSON error).
	if code, _, _ = httpDo(t, h, "POST", "/tool/search", ``); code == 404 || code == 405 {
		t.Fatalf("empty body = %d", code)
	}
}

func TestMemdHTTPBodyCap(t *testing.T) {
	db, _ := openTestMemd(t)
	big := `{"text":"` + strings.Repeat("a", memdMaxBody+10) + `"}`
	code, _, _ := httpDo(t, memdHandler(db), "POST", "/tool/save_memory", big)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize body = %d, want 413", code)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM observations`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("oversize body stored something: n=%d err=%v", n, err)
	}
}
