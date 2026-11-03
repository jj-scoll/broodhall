package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Fixtures are synthetic.

func serveClient(sock string) *http.Client {
	return &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
}

func serveDo(c *http.Client, method, path, body string) (int, string, error) {
	req, err := http.NewRequest(method, "http://memory"+path, strings.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

func TestMemdServeEndToEnd(t *testing.T) {
	fastWorkers(t)
	xdg := shortSockDir(t)
	t.Setenv("XDG_RUNTIME_DIR", xdg)
	dir := t.TempDir()
	root := t.TempDir()
	proj := filepath.Join(root, "-work-demo")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	sc := newScripted(t, func(int, string) fakeReply { return result(t, goodObs(), "") })

	uiAddr := freeLoopbackAddr(t)
	oldUI, oldQ := memdUIAddr, memdQuackURI
	memdUIAddr = uiAddr
	qURI, _ := freeQuackURI(t)
	memdQuackURI = qURI
	t.Cleanup(func() { memdUIAddr, memdQuackURI = oldUI, oldQ })

	var served *sql.DB
	memdServeDBHook = func(db *sql.DB) { served = db }
	t.Cleanup(func() { memdServeDBHook = nil })

	// The daemon takes its skip lists from settings.json at MEMORY_SETTINGS.
	cmDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cmDir, "settings.json"), []byte(`{"skip_tools": "Monitor"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEMORY_SETTINGS", filepath.Join(cmDir, "settings.json"))
	origTools, origBash := skipTools, skipBashPatterns
	t.Cleanup(func() { skipTools, skipBashPatterns = origTools, origBash })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- memdServe(ctx, dir, map[string]string{root: "acct"}) }()

	sock := memdSockPath()
	c := serveClient(sock)
	waitFor(t, "healthz over the socket", func() bool {
		code, _, err := serveDo(c, "GET", "/healthz", "")
		return err == nil && code == 200
	})
	if !shouldSkip("Monitor", "") || shouldSkip("TodoWrite", "") {
		t.Error("memdServe did not apply skip_tools from settings.json")
	}
	if fi, err := os.Stat(sock); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket stat = %v, %v; want mode 600", fi, err)
	}

	// The tailer drops events stamped before startup, so stamp them now.
	now := time.Now().UTC()
	ts := func(d time.Duration) string { return now.Add(d).Format("2006-01-02T15:04:05.000Z") }
	start := time.Now()
	tailAppend(t, filepath.Join(proj, "sess-e2e.jsonl"),
		tailPrompt("sess-e2e", "fix the parser", ts(0), "cli")+
			tailToolUse("sess-e2e", "tu1", "Edit", ts(time.Second))+tailResult("sess-e2e", "tu1", ts(time.Second)))

	var lat time.Duration
	waitFor(t, "observation searchable over the socket", func() bool {
		code, body, err := serveDo(c, "POST", "/tool/search", `{"query":"nil pointer"}`)
		if err == nil && code == 200 && strings.Contains(body, "Fixed nil pointer in parser") {
			lat = time.Since(start)
			return true
		}
		return false
	})
	t.Logf("pipeline latency (file write -> searchable, fake runClaude, 2ms worker poll): %v; model calls: %d", lat, sc.calls())
	if lat > 5*time.Second {
		t.Errorf("latency %v exceeds 5s", lat)
	}

	// UI listener: loopback, answers the health endpoint.
	waitFor(t, "UI health on 127.0.0.1", func() bool {
		resp, err := http.Get("http://" + uiAddr + "/api/v1/health")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})
	// Quack came up (the extensions are present in this environment).
	if _, err := os.Stat(filepath.Join(dir, "quack.token")); err != nil {
		t.Errorf("quack.token: %v", err)
	}
	// mem mcp talks to the live socket.
	var mcpOut strings.Builder
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n"
	if rc := memMCP(strings.NewReader(in), &mcpOut, sock); rc != 0 || !strings.Contains(mcpOut.String(), `"search"`) {
		t.Errorf("memMCP rc=%d out=%.300s", rc, mcpOut.String())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("memdServe returned %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("memdServe did not return after cancel")
	}
	if _, err := os.Lstat(sock); !os.IsNotExist(err) {
		t.Errorf("socket file still present: %v", err)
	}
	if c, err := net.DialTimeout("tcp", uiAddr, 300*time.Millisecond); err == nil {
		c.Close()
		t.Error("UI listener still accepting after shutdown")
	}
	if c, err := net.DialTimeout("tcp", strings.TrimPrefix(qURI, "quack:"), 300*time.Millisecond); err == nil {
		c.Close()
		t.Error("Quack listener still accepting after shutdown")
	}
	if served == nil {
		t.Fatal("DB hook never ran")
	}
	if err := served.Ping(); err == nil {
		t.Error("DB still open after memdServe returned")
	}
	// The state dir is reusable: the DB reopens and holds the observation.
	db, err := openMemdDB(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	if n := countQ(t, db, `SELECT count(*) FROM observations`); n != 1 {
		t.Errorf("observations = %d, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "memory.log")); err != nil {
		t.Errorf("memory.log: %v", err)
	}
}

func TestMemdServeNeedsRuntimeDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	if err := memdServe(context.Background(), t.TempDir(), nil); err == nil {
		t.Fatal("want an error without XDG_RUNTIME_DIR")
	}
}

// R31: the tailer writes only while holding memdWriteMu.
func TestMemdTailerTakesWriteMutex(t *testing.T) {
	e := newTailEnv(t, "")
	tl := e.tailer()
	tailAppend(t, e.path("s.jsonl"), tailPrompt("s", "hi", tailTS(10, 0, 0), "cli"))

	memdWriteMu.Lock()
	done := make(chan struct{})
	go func() { tl.Scan(); close(done) }()
	select {
	case <-done:
		memdWriteMu.Unlock()
		t.Fatal("Scan wrote while memdWriteMu was held")
	case <-time.After(200 * time.Millisecond):
	}
	memdWriteMu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Scan did not finish after the mutex was released")
	}
	if n := tailCount(t, e.db, `SELECT count(*) FROM prompts`); n != 1 {
		t.Errorf("prompts = %d, want 1", n)
	}
}

// Tailer, workers and the summary worker write at once; none may see a DuckDB conflict.
func TestMemdConcurrentWritersNoConflict(t *testing.T) {
	fastWorkers(t)
	e := newTailEnv(t, "")
	newScripted(t, func(int, string) fakeReply { return result(t, goodObs(), "") })

	var conflicts atomic.Int64
	oldLog := workerLogf
	workerLogf = func(f string, a ...any) {
		if strings.Contains(strings.ToLower(fmt.Sprintf(f, a...)), "conflict") {
			conflicts.Add(1)
		}
	}
	t.Cleanup(func() { workerLogf = oldLog })

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); runObserverWorkers(ctx, e.db, 2) }()
	go func() { defer wg.Done(); runSummaryWorker(ctx, e.db) }()

	tl := e.tailer()
	const files, per = 6, 8
	for round := 0; round < per; round++ {
		for f := 0; f < files; f++ {
			sess := fmt.Sprintf("s%d", f)
			id := fmt.Sprintf("tu-%d-%d", f, round)
			ts := tailTS(10, round, f)
			tailAppend(t, e.path(sess+".jsonl"), tailPrompt(sess, "p", ts, "cli")+tailToolUse(sess, id, "Edit", ts)+tailResult(sess, id, ts))
		}
		if _, err := tl.Scan(); err != nil {
			t.Fatalf("Scan round %d: %v", round, err)
		}
	}
	waitFor(t, "queue drained", func() bool {
		return countQ(t, e.db, `SELECT count(*) FROM queue WHERE state IN ('queued','processing')`) == 0
	})
	cancel()
	wg.Wait()
	if n := conflicts.Load(); n != 0 {
		t.Errorf("%d conflict errors logged", n)
	}
}

func TestRotatingLog(t *testing.T) {
	p := filepath.Join(t.TempDir(), "memory.log")
	l, err := openRotatingLog(p, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	line := strings.Repeat("a", 59) + "\n" // 60 bytes
	for i := 0; i < 5; i++ {
		if _, err := l.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	cur, _ := os.ReadFile(p)
	old, err := os.ReadFile(p + ".1")
	if err != nil {
		t.Fatalf("no backup: %v", err)
	}
	if len(cur) > 100 || len(old) > 100 {
		t.Errorf("sizes cur=%d old=%d, want <= 100", len(cur), len(old))
	}
	if _, err := os.Stat(p + ".2"); err == nil {
		t.Error("a second backup exists")
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("log mode = %o, want 600", fi.Mode().Perm())
	}
}

func TestByFileDeadline(t *testing.T) {
	db, _ := openTestMemd(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := byFileCtx(ctx, db, "a.go", ""); err == nil {
		t.Fatal("byFileCtx ignored a cancelled context")
	}
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// Quack failing to start (no extension dir) is logged; memory keeps serving.
func TestMemdServeSurvivesQuackFailure(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", shortSockDir(t))
	t.Setenv("MEMORY_EXT_DIR", t.TempDir()) // no extensions: startQuack must fail
	dir := t.TempDir()
	// openMemdDB needs fts; give the empty ext dir a copy via the real one.
	real := filepath.Join(testExtDir, "fts.duckdb_extension")
	if b, err := os.ReadFile(real); err == nil {
		os.WriteFile(filepath.Join(os.Getenv("MEMORY_EXT_DIR"), "fts.duckdb_extension"), b, 0o644)
	} else {
		t.Skip("fts extension unavailable")
	}
	uiAddr := freeLoopbackAddr(t)
	old := memdUIAddr
	memdUIAddr = uiAddr
	t.Cleanup(func() { memdUIAddr = old })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- memdServe(ctx, dir, nil) }()
	c := serveClient(memdSockPath())
	waitFor(t, "healthz without quack", func() bool {
		code, _, err := serveDo(c, "GET", "/healthz", "")
		return err == nil && code == 200
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("memdServe = %v, want nil", err)
	}
}
