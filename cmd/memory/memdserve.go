package main

// memory daemon: `memory serve` wires the DB, tailer, observer workers, summary worker and the
// unix-socket HTTP server, and stops them in order on SIGTERM or context cancellation.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	memdLogMax          = 10 << 20
	memdShutdownBudget  = 10 * time.Second
	memdByFileDeadline  = 100 * time.Millisecond
	memdReadHeaderLimit = 5 * time.Second
	memdReadLimit       = 15 * time.Second
	memdWriteLimit      = 30 * time.Second
)

// memdUIAddr is the UI listener: loopback only. Neither the brief nor the spec names a port;
// 9495 sits beside Quack's 9494. Tests override it.
var memdUIAddr = "127.0.0.1:9495"

// memdServeDBHook, when set (tests), receives the opened DB.
var memdServeDBHook func(*sql.DB)

// claudeConfigDir is Claude Code's config dir: $CLAUDE_CONFIG_DIR, default ~/.claude.
func claudeConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// defaultMemdRoots maps transcript root -> account. The one root is the config dir's
// projects dir, account "default".
func defaultMemdRoots() map[string]string {
	return map[string]string{filepath.Join(claudeConfigDir(), "projects"): "default"}
}

// memdServe runs the daemon on state dir dir, tailing roots (transcript root -> account),
// until ctx is cancelled. It returns nil after a clean stop: listener closed, socket file
// removed, tailer and workers finished, DB closed.
func memdServe(ctx context.Context, dir string, roots map[string]string) error {
	if os.Getenv("XDG_RUNTIME_DIR") == "" {
		return errors.New("XDG_RUNTIME_DIR is not set; the daemon socket lives there")
	}
	// The socket is bound first: listenSocket changes the process-wide umask, so no other
	// goroutine may be creating files (WAL, log, workers) at that moment.
	sock := memdSockPath()
	ln, err := listenSocket(sock)
	if err != nil {
		return err
	}
	defer os.Remove(sock)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		ln.Close()
		return fmt.Errorf("memory dir: %w", err)
	}
	lg, err := openRotatingLog(filepath.Join(dir, "memory.log"), memdLogMax)
	if err != nil {
		ln.Close()
		return err
	}
	defer lg.Close()
	restore := redirectLogs(lg)
	defer restore()

	// Before any worker starts: the workers read these without a lock.
	settings := memdSettingsPath()
	skipTools, skipBashPatterns = loadSkipSettings(settings)
	log.Printf("memory: skip lists from %s: %d tools, %d bash patterns", settings, len(skipTools), len(skipBashPatterns))

	db, err := openMemdDB(dir)
	if err != nil {
		ln.Close()
		return err
	}
	if memdServeDBHook != nil {
		memdServeDBHook(db)
	}
	defer db.Close()

	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	srv := &http.Server{
		Handler:           memdHandler(db),
		ReadHeaderTimeout: memdReadHeaderLimit,
		ReadTimeout:       memdReadLimit,
		WriteTimeout:      memdWriteLimit,
	}
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.Serve(ln) }()

	var wg sync.WaitGroup
	tailer := newMemdTailer(db, roots, time.Now())
	wg.Add(4)
	// The in-memory search index builds in the background; searches use SQL until it is ready.
	go func() { defer wg.Done(); searcherFor(db).buildAll(runCtx) }()
	go func() {
		defer wg.Done()
		if err := tailer.Run(runCtx); err != nil {
			log.Printf("memory tail: %v", err)
		}
	}()
	go func() { defer wg.Done(); runObserverWorkers(runCtx, db, 0) }()
	go func() { defer wg.Done(); runSummaryWorker(runCtx, db) }()

	// UI: loopback TCP. No WriteTimeout, which would cut the SSE stream at /api/v1/events (R39).
	// A failed bind is logged and memory keeps serving the socket.
	var uiSrv *http.Server
	if uln, err := net.Listen("tcp", memdUIAddr); err != nil {
		log.Printf("memory ui: not serving: %v", err)
	} else {
		uiSrv = &http.Server{Handler: uiHandler(db), ReadHeaderTimeout: memdReadHeaderLimit}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := uiSrv.Serve(uln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("memory ui: %v", err)
			}
		}()
		log.Printf("memory: UI on http://%s/", uln.Addr())
	}
	// Quack: failure is logged by startQuack; memory keeps running without it.
	quackUp := startQuack(db, dir) == nil
	// Later: memory import (Task 22) is a separate subcommand, not part of the daemon.
	log.Printf("memory: serving on %s, state in %s", sock, dir)

	var serveErr error
	select {
	case <-runCtx.Done():
	case err := <-srvErr:
		serveErr = fmt.Errorf("http serve: %w", err)
	}
	stop()
	sctx, cancel := context.WithTimeout(context.Background(), memdShutdownBudget)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		srv.Close()
	}
	if uiSrv != nil {
		if err := uiSrv.Shutdown(sctx); err != nil {
			uiSrv.Close() // open SSE streams outlive the grace period
		}
	}
	if quackUp {
		if _, err := db.Exec("SELECT * FROM quack_stop(" + sqlQuote(memdQuackURI) + ")"); err != nil {
			log.Printf("memory quack: stop: %v", err)
		}
	}
	wg.Wait()
	log.Printf("memory: stopped")
	return serveErr
}

// rotatingLog is an io.WriteCloser over one log file capped at max bytes: a write that would
// pass the cap first moves the file to path+".1" (replacing any older backup).
type rotatingLog struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

func openRotatingLog(path string, max int64) (*rotatingLog, error) {
	l := &rotatingLog{path: path, max: max}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *rotatingLog) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, fi.Size()
	return nil
}

func (l *rotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return 0, os.ErrClosed
	}
	if l.size > 0 && l.size+int64(len(p)) > l.max {
		l.f.Close()
		os.Rename(l.path, l.path+".1")
		if err := l.open(); err != nil {
			l.f = nil
			return 0, err
		}
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

func (l *rotatingLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// redirectLogs sends the standard logger and os.Stderr (the tailer prints there) to w and
// returns a function that restores both and waits for the stderr copier to finish.
func redirectLogs(w io.Writer) (restore func()) {
	oldOut, oldErr := log.Writer(), os.Stderr
	pr, pw, err := os.Pipe()
	if err != nil {
		log.SetOutput(w)
		return func() { log.SetOutput(oldOut) }
	}
	done := make(chan struct{})
	go func() { io.Copy(w, pr); close(done) }()
	log.SetOutput(w)
	os.Stderr = pw
	return func() {
		os.Stderr = oldErr
		log.SetOutput(oldOut)
		pw.Close()
		<-done
		pr.Close()
	}
}
