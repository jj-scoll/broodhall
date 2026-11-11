package main

// handover: a new plugin version replaces the running daemon of an older one. The launcher
// runs `memory handover` before it starts `memory serve`; the new binary asks the old daemon,
// over the daemon's own 0600 unix socket (so only the same user can), to finish and exit.
// Nothing here matches on process names, and no signal goes to a pid that the daemon did not
// report itself. `memory gc` then removes install dirs of old versions.
//
// Every failure leaves the old daemon running or starts nothing new; the launcher starts
// `memory serve` afterwards either way, and that exits at once while the socket is taken.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	memdHandoverBudget = 8 * time.Second  // under the SessionStart hook's 15 s, after the 3 s context call
	memdGCHealthWait   = 30 * time.Second // how long gc waits for the new daemon before giving up
)

// memdInfo is the body of GET /version.
type memdInfo struct {
	Version string `json:"version"`
	PID     int    `json:"pid"`
}

// handoverOutcome says what memdHandover did.
type handoverOutcome int

const (
	handoverNoDaemon handoverOutcome = iota // nothing answers on the socket
	handoverKept                            // same or newer daemon, or a version we cannot compare: left alone
	handoverReplaced                        // an older daemon was asked to stop and is gone
	handoverLegacy                          // older daemon without /shutdown: left running
	handoverStuck                           // older daemon did not stop in time: left running
	handoverBusy                            // another launcher holds the handover lock
)

var (
	// memdRequestStop makes the running daemon stop; memdServe sets it to its cancel func.
	memdRequestStop = func() {}
	// memdPidAlive reports whether pid exists. A var so tests need no real process.
	memdPidAlive = func(pid int) bool {
		err := syscall.Kill(pid, 0)
		return err == nil || errors.Is(err, syscall.EPERM)
	}
	// memdOwnVersion is this binary's release number, "" for an untagged build.
	memdOwnVersion = func() string {
		bi, ok := debug.ReadBuildInfo()
		return buildVersion(bi, ok)
	}
)

// registerControl adds the handover endpoints to the daemon's socket API:
//
//	GET  /version   200, {"version":"1.10.8","pid":1234}; version is "" for an untagged build
//	POST /shutdown  202, then the daemon stops as on SIGTERM: workers stop claiming work, the
//	                queue stays in DuckDB, the DB is checkpointed on close, the process exits
//
// The socket is mode 0600 in the user's runtime dir, so only that user reaches these.
func registerControl(mux *http.ServeMux) {
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, memdInfo{Version: memdOwnVersion(), PID: os.Getpid()})
	})
	mux.HandleFunc("POST /shutdown", func(w http.ResponseWriter, r *http.Request) {
		writeText(w, 202, "stopping")
		go memdRequestStop()
	})
}

var semverRe = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)

// parseSemver parses "x.y.z"; anything else (a prerelease, "dev", "") is not comparable.
func parseSemver(v string) (n [3]int, ok bool) {
	m := semverRe.FindStringSubmatch(v)
	if m == nil {
		return n, false
	}
	for i := range n {
		var err error
		if n[i], err = strconv.Atoi(m[i+1]); err != nil {
			return n, false
		}
	}
	return n, true
}

func semverLess(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// memdHandoverLock takes the per-user handover lock, polling for up to wait. flock dies with
// its process, so a killed launcher never leaves a stale lock.
func memdHandoverLock(wait time.Duration) (unlock func(), ok bool) {
	f, err := os.OpenFile(filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "memory.handover.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false
	}
	deadline := time.Now().Add(wait)
	for {
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			return func() { f.Close() }, true
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, false
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// memdHandover makes room for the daemon of version own. It looks at the daemon on sock and,
// when that one is older, asks it to stop and waits up to budget for it to be gone. The note
// is one line for the user, set when an older daemon stays running.
func memdHandover(sock, own string, budget time.Duration) (handoverOutcome, string) {
	start := time.Now()
	unlock, ok := memdHandoverLock(budget)
	if !ok {
		return handoverBusy, "" // another session is handing over; it starts the daemon
	}
	defer unlock()

	status, body, err := memGetStatus(sock, "/version", time.Second)
	switch {
	case err != nil:
		return handoverNoDaemon, ""
	case status == http.StatusNotFound:
		return handoverLegacy, "an older memory daemon without handover support is still running; to switch to the new version run `pkill -f 'memory serve'` (it stops every memory daemon of yours), then start a new session"
	case status != http.StatusOK:
		return handoverKept, ""
	}
	var info memdInfo
	if json.Unmarshal(body, &info) != nil {
		return handoverKept, ""
	}
	mine, ok1 := parseSemver(own)
	theirs, ok2 := parseSemver(info.Version)
	if !ok1 || !ok2 || !semverLess(theirs, mine) {
		return handoverKept, "" // same, newer, or not comparable: never replace
	}

	// A reply that never comes is a daemon dying under us; waiting for it to be gone covers both.
	memRequest(http.MethodPost, sock, "/shutdown", 2*time.Second)
	for {
		if memdDaemonGone(sock, info.PID) {
			return handoverReplaced, ""
		}
		if time.Since(start) >= budget {
			return handoverStuck, fmt.Sprintf("memory daemon %s (pid %d) did not stop within %s and keeps running; run `kill %d`, then start a new session",
				info.Version, info.PID, budget.Round(100*time.Millisecond), info.PID)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// memdDaemonGone is true when nothing listens on sock and pid (when known) has exited.
func memdDaemonGone(sock string, pid int) bool {
	if c, err := net.DialTimeout("unix", sock, 200*time.Millisecond); err == nil {
		c.Close()
		return false
	}
	return pid <= 0 || !memdPidAlive(pid)
}

// memdWaitHealthy polls sock until the daemon there reports version ver.
func memdWaitHealthy(sock, ver string, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for {
		if status, body, err := memGetStatus(sock, "/version", time.Second); err == nil && status == http.StatusOK {
			var info memdInfo
			if json.Unmarshal(body, &info) == nil && info.Version == ver {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// memHandoverCmd is `memory handover`. Exit 0 means the launcher can go on to start the
// daemon; exit 3 means an older daemon stays running and out holds the one-line note.
func memHandoverCmd(out io.Writer) int {
	if os.Getenv("XDG_RUNTIME_DIR") == "" {
		return 0
	}
	_, note := memdHandover(memdSockPath(), memdOwnVersion(), memdHandoverBudget)
	if note != "" {
		fmt.Fprintln(out, note)
		return 3
	}
	return 0
}

// memdGC removes the version dirs under binDir that are older than the previous version: it
// keeps current, the next lower installed version, every newer one (another account may run
// it), anything that is not an x.y.z dir, and any dir that a running process (a line of
// running, as `ps` prints the arguments) was started from. It returns the names removed.
func memdGC(binDir, current string, running []string) (removed []string, err error) {
	cur, ok := parseSemver(current)
	if !ok {
		return nil, nil
	}
	ents, err := os.ReadDir(binDir)
	if err != nil {
		return nil, err
	}
	type ver struct {
		name string
		n    [3]int
	}
	var older []ver
	for _, e := range ents {
		if n, ok := parseSemver(e.Name()); ok && e.IsDir() && semverLess(n, cur) {
			older = append(older, ver{e.Name(), n})
		}
	}
	prev := -1
	for i, v := range older {
		if prev < 0 || semverLess(older[prev].n, v.n) {
			prev = i
		}
	}
	var errs []error
	for i, v := range older {
		dir := filepath.Join(binDir, v.name)
		if i == prev || inUse(dir, running) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, v.name)
	}
	return removed, errors.Join(errs...)
}

// inUse reports whether a running process was started from a file under dir.
func inUse(dir string, running []string) bool {
	for _, l := range running {
		if strings.HasPrefix(l, dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// memdRunningLines is the argument line of every process, from ps (Linux and macOS).
func memdRunningLines() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "args=").Output()
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

// memGCCmd is `memory gc <bin dir> <current version>`. It waits for this version's daemon to
// answer, then removes old version dirs. It skips the removal when it cannot list processes.
func memGCCmd(args []string) int {
	if len(args) != 2 || os.Getenv("XDG_RUNTIME_DIR") == "" {
		return 0
	}
	binDir, current := args[0], args[1]
	if !memdWaitHealthy(memdSockPath(), current, memdGCHealthWait) {
		fmt.Printf("memory gc: daemon %s not healthy; keeping old versions\n", current)
		return 0
	}
	running, err := memdRunningLines()
	if err != nil {
		fmt.Printf("memory gc: cannot list processes (%v); keeping old versions\n", err)
		return 0
	}
	removed, err := memdGC(binDir, current, running)
	if len(removed) > 0 {
		fmt.Printf("memory gc: removed %s from %s\n", strings.Join(removed, " "), binDir)
	}
	if err != nil {
		fmt.Printf("memory gc: %v\n", err)
	}
	return 0
}
