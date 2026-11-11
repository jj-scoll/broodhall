package main

// Handover tests run against fake daemons on temp unix sockets (XDG_RUNTIME_DIR is a temp
// dir); no real daemon, port or database is touched.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDaemon serves the daemon's control API on a unix socket in dir.
type fakeDaemon struct {
	sock      string
	srv       *http.Server
	shutdowns atomic.Int32
	alive     atomic.Bool // what memdPidAlive reports for its pid
}

// startFake starts a fake daemon. version "" means a legacy daemon (no /version, no
// /shutdown). dieOnShutdown makes it stop answering and exit after /shutdown; dieMidReply
// closes the connection without replying to /shutdown, as a daemon that dies mid-handover.
func startFake(t *testing.T, dir, version string, dieOnShutdown, dieMidReply bool) *fakeDaemon {
	t.Helper()
	f := &fakeDaemon{sock: filepath.Join(dir, "memory.sock")}
	f.alive.Store(true)
	ln, err := net.Listen("unix", f.sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	if version != "" {
		mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(memdInfo{Version: version, PID: 4242})
		})
		mux.HandleFunc("POST /shutdown", func(w http.ResponseWriter, r *http.Request) {
			f.shutdowns.Add(1)
			if dieMidReply {
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close()
			} else {
				w.WriteHeader(202)
			}
			if dieOnShutdown {
				go func() {
					time.Sleep(50 * time.Millisecond)
					f.srv.Close()
					f.alive.Store(false)
				}()
			}
		})
	}
	f.srv = &http.Server{Handler: mux}
	go f.srv.Serve(ln)
	t.Cleanup(func() { f.srv.Close() })
	return f
}

func TestMemdHandoverCases(t *testing.T) {
	tests := []struct {
		name          string
		daemon        string // "" = legacy
		own           string
		die, midReply bool
		want          handoverOutcome
		wantShutdowns int32
		noteContains  string
	}{
		{"same version is a no-op", "1.10.8", "1.10.8", true, false, handoverKept, 0, ""},
		{"newer daemon is never replaced by an older launcher", "1.11.0", "1.10.8", true, false, handoverKept, 0, ""},
		{"older daemon with the mechanism is handed over", "1.10.7", "1.10.8", true, false, handoverReplaced, 1, ""},
		{"1.9.9 is older than 1.10.0 (numeric compare)", "1.9.9", "1.10.0", true, false, handoverReplaced, 1, ""},
		{"daemon dies mid-handover counts as gone", "1.10.7", "1.10.8", true, true, handoverReplaced, 1, ""},
		{"older daemon that ignores the request is reported stuck", "1.10.7", "1.10.8", false, false, handoverStuck, 1, "did not stop"},
		{"legacy daemon without the mechanism is left running with one line", "", "1.10.8", false, false, handoverLegacy, 0, "pkill"},
		{"dev launcher never replaces", "1.10.7", "", true, false, handoverKept, 0, ""},
		{"unparsable daemon version is left alone", "garbage", "1.10.8", true, false, handoverKept, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := shortSockDir(t)
			t.Setenv("XDG_RUNTIME_DIR", dir)
			f := startFake(t, dir, tt.daemon, tt.die, tt.midReply)
			old := memdPidAlive
			memdPidAlive = func(int) bool { return f.alive.Load() }
			t.Cleanup(func() { memdPidAlive = old })

			got, note := memdHandover(memdSockPath(), tt.own, 600*time.Millisecond)
			if got != tt.want {
				t.Errorf("outcome = %d (%q), want %d", got, note, tt.want)
			}
			if n := f.shutdowns.Load(); n != tt.wantShutdowns {
				t.Errorf("shutdown requests = %d, want %d", n, tt.wantShutdowns)
			}
			if tt.noteContains != "" && !strings.Contains(note, tt.noteContains) {
				t.Errorf("note = %q, want it to contain %q", note, tt.noteContains)
			}
			if tt.noteContains == "" && note != "" {
				t.Errorf("unexpected note %q", note)
			}
			if strings.Count(note, "\n") > 1 {
				t.Errorf("note must be one line: %q", note)
			}
		})
	}
}

func TestMemdHandoverNoDaemon(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", shortSockDir(t))
	if got, _ := memdHandover(memdSockPath(), "1.10.8", time.Second); got != handoverNoDaemon {
		t.Errorf("outcome = %d, want handoverNoDaemon", got)
	}
	// A stale socket file nobody listens on is the same as no daemon.
	ln, _ := net.Listen("unix", memdSockPath())
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if got, _ := memdHandover(memdSockPath(), "1.10.8", time.Second); got != handoverNoDaemon {
		t.Errorf("stale socket: outcome = %d, want handoverNoDaemon", got)
	}
}

// Two sessions starting at once: exactly one asks the daemon to stop; the other waits on the
// lock, then finds no older daemon.
func TestMemdHandoverTwoLaunchers(t *testing.T) {
	dir := shortSockDir(t)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	f := startFake(t, dir, "1.10.7", true, false)
	old := memdPidAlive
	memdPidAlive = func(int) bool { return f.alive.Load() }
	t.Cleanup(func() { memdPidAlive = old })

	var wg sync.WaitGroup
	res := make([]handoverOutcome, 2)
	for i := range res {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res[i], _ = memdHandover(memdSockPath(), "1.10.8", 3*time.Second)
		}()
	}
	wg.Wait()
	if n := f.shutdowns.Load(); n != 1 {
		t.Errorf("shutdown requests = %d, want exactly 1", n)
	}
	sort.Slice(res, func(i, j int) bool { return res[i] < res[j] })
	if res[0] != handoverNoDaemon || res[1] != handoverReplaced {
		t.Errorf("outcomes = %v, want one replaced and one no-daemon", res)
	}
}

// A launcher that cannot get the lock in time leaves the work to the one holding it.
func TestMemdHandoverBusy(t *testing.T) {
	dir := shortSockDir(t)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	startFake(t, dir, "1.10.7", true, false)
	unlock, ok := memdHandoverLock(100 * time.Millisecond)
	if !ok {
		t.Fatal("could not take the lock")
	}
	defer unlock()
	if got, _ := memdHandover(memdSockPath(), "1.10.8", 200*time.Millisecond); got != handoverBusy {
		t.Errorf("outcome = %d, want handoverBusy", got)
	}
}

// The daemon side: GET /version reports build version and pid; POST /shutdown asks the
// daemon to stop and replies 202 first.
func TestMemdControlHandler(t *testing.T) {
	mux := http.NewServeMux()
	registerControl(mux)
	var stopped atomic.Int32
	old := memdRequestStop
	memdRequestStop = func() { stopped.Add(1) }
	t.Cleanup(func() { memdRequestStop = old })

	get := func(method, path string) (int, string) {
		rec := &respRec{h: http.Header{}}
		req, _ := http.NewRequest(method, path, nil)
		mux.ServeHTTP(rec, req)
		return rec.code, rec.body.String()
	}
	code, body := get("GET", "/version")
	var info memdInfo
	if code != 200 || json.Unmarshal([]byte(body), &info) != nil || info.PID != os.Getpid() {
		t.Errorf("GET /version = %d %q, want 200 with pid %d", code, body, os.Getpid())
	}
	if code, _ := get("GET", "/shutdown"); code != 405 {
		t.Errorf("GET /shutdown = %d, want 405", code)
	}
	if stopped.Load() != 0 {
		t.Fatal("a GET stopped the daemon")
	}
	if code, _ := get("POST", "/shutdown"); code != 202 {
		t.Errorf("POST /shutdown = %d, want 202", code)
	}
	waitFor(t, "stop requested", func() bool { return stopped.Load() == 1 })
}

type respRec struct {
	h    http.Header
	code int
	body strings.Builder
}

func (r *respRec) Header() http.Header { return r.h }
func (r *respRec) WriteHeader(c int)   { r.code = c }
func (r *respRec) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = 200
	}
	return r.body.Write(b)
}

// End to end against the real daemon wiring, with temp dirs and ephemeral ports: a handover
// request makes memdServe stop cleanly.
func TestMemdServeShutdownEndpoint(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", shortSockDir(t))
	oldUI, oldQ := memdUIAddr, memdQuackURI
	memdUIAddr = freeLoopbackAddr(t)
	qURI, _ := freeQuackURI(t)
	memdQuackURI = qURI
	t.Cleanup(func() { memdUIAddr, memdQuackURI = oldUI, oldQ })

	done := make(chan error, 1)
	go func() { done <- memdServe(context.Background(), t.TempDir(), map[string]string{}) }()
	c := serveClient(memdSockPath())
	waitFor(t, "daemon up", func() bool {
		code, _, err := serveDo(c, "GET", "/healthz", "")
		return err == nil && code == 200
	})
	if code, body, err := serveDo(c, "GET", "/version", ""); err != nil || code != 200 || !strings.Contains(body, `"pid"`) {
		t.Fatalf("GET /version = %d %q %v", code, body, err)
	}
	if code, _, err := serveDo(c, "POST", "/shutdown", ""); err != nil || code != 202 {
		t.Fatalf("POST /shutdown = %d %v", code, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("memdServe = %v, want a clean stop", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not stop after /shutdown")
	}
	if _, err := os.Stat(memdSockPath()); !os.IsNotExist(err) {
		t.Errorf("socket still present: %v", err)
	}
}

func mkBin(t *testing.T, vers ...string) string {
	t.Helper()
	bin := t.TempDir()
	for _, v := range vers {
		if err := os.MkdirAll(filepath.Join(bin, v), 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(bin, v, "memory"), []byte("x"), 0o755)
	}
	return bin
}

func present(t *testing.T, bin string) string {
	t.Helper()
	es, _ := os.ReadDir(bin)
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return strings.Join(names, " ")
}

func TestMemdGC(t *testing.T) {
	all := []string{"1.10.2", "1.10.3", "1.10.5", "1.10.6", "1.10.7", "1.9.0", "1.10.8", ".download.abc", ".fail-1.10.7", "notes"}
	tests := []struct {
		name    string
		current string
		running []string // ps argument lines
		want    string
	}{
		{"keeps current and previous, newer, and non-version names", "1.10.7",
			nil, ".download.abc .fail-1.10.7 1.10.6 1.10.7 1.10.8 notes"},
		{"never removes the dir of a running binary", "1.10.7",
			[]string{"BIN/1.10.2/memory serve", "/usr/bin/other", "BIN/1.10.5/memory mcp"},
			".download.abc .fail-1.10.7 1.10.2 1.10.5 1.10.6 1.10.7 1.10.8 notes"},
		{"a similar prefix of a path is not the same dir", "1.10.7",
			[]string{"BIN/1.10.20/memory serve", "BIN2/1.10.2/memory serve"},
			".download.abc .fail-1.10.7 1.10.6 1.10.7 1.10.8 notes"},
		{"previous is the next lower installed version, numerically", "1.10.2",
			nil, ".download.abc .fail-1.10.7 1.10.2 1.10.3 1.10.5 1.10.6 1.10.7 1.10.8 1.9.0 notes"},
		{"unparsable current removes nothing", "dev", nil,
			".download.abc .fail-1.10.7 1.10.2 1.10.3 1.10.5 1.10.6 1.10.7 1.10.8 1.9.0 notes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := mkBin(t, all...)
			os.WriteFile(filepath.Join(bin, "notes"), []byte("keep"), 0o644)
			var running []string
			for _, l := range tt.running {
				running = append(running, strings.ReplaceAll(l, "BIN2", bin+"2"))
			}
			for i, l := range running {
				running[i] = strings.ReplaceAll(l, "BIN/", bin+"/")
			}
			if _, err := memdGC(bin, tt.current, running); err != nil {
				t.Fatal(err)
			}
			if got := present(t, bin); got != tt.want {
				t.Errorf("left = %q\nwant   %q", got, tt.want)
			}
		})
	}
}

func TestMemdWaitHealthy(t *testing.T) {
	dir := shortSockDir(t)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	if memdWaitHealthy(memdSockPath(), "1.10.8", 200*time.Millisecond) {
		t.Error("healthy with no daemon")
	}
	startFake(t, dir, "1.10.7", false, false)
	if memdWaitHealthy(memdSockPath(), "1.10.8", 300*time.Millisecond) {
		t.Error("an older daemon is not the new one")
	}
	if !memdWaitHealthy(memdSockPath(), "1.10.7", 300*time.Millisecond) {
		t.Error("matching daemon not reported healthy")
	}
}
