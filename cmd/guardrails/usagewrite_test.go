package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// usagePath returns a cache file path inside a fresh directory.
func usagePath(t *testing.T) (path, dir string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "widget")
	return filepath.Join(dir, "usage.json"), dir
}

func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, "usage.json.*"))
	var out []string
	for _, p := range m {
		if !strings.HasSuffix(p, ".lock") {
			out = append(out, filepath.Base(p))
		}
	}
	return out
}

// A failed move into place must not leave usage.json.<pid> behind.
func TestWriteUsageCacheNoOrphanOnRenameError(t *testing.T) {
	path, dir := usagePath(t)
	// a directory where the cache file belongs makes the rename fail
	if err := os.MkdirAll(filepath.Join(dir, "usage.json", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeUsageCache(path, "10", "5", "100", "6", "200")
	if got := leftovers(t, dir); len(got) != 0 {
		t.Errorf("orphan temp files after failed write: %v", got)
	}
}

// Concurrent sessions in one window: the highest five-hour value must win, not
// whichever read-modify-write finished last.
func TestWriteUsageCacheConcurrentKeepsMax(t *testing.T) {
	path, dir := usagePath(t)
	const n = 40
	for round := 0; round < 3; round++ {
		os.RemoveAll(dir)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 1; i <= n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				writeUsageCache(path, "10", fmt.Sprint(i), "100", "1", "200")
			}(i)
		}
		close(start)
		wg.Wait()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf(`"five":%d,`, n); !strings.Contains(string(b), want) {
			t.Fatalf("round %d: lost update, got %s want five=%d", round, b, n)
		}
		if got := leftovers(t, dir); len(got) != 0 {
			t.Fatalf("round %d: orphans %v", round, got)
		}
	}
}

// A writer stuck holding the lock (a disk stall) must not stall every other session's
// statusline behind it: past a short wait the write goes ahead unlocked, as before locking.
func TestWriteUsageCacheLockHeldDoesNotBlock(t *testing.T) {
	path, dir := usagePath(t)
	os.MkdirAll(dir, 0o755)
	lk, err := os.OpenFile(filepath.Join(dir, "usage.json.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Close()
	if err := syscall.Flock(int(lk.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { writeUsageCache(path, "10", "7", "100", "6", "200"); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("writeUsageCache still waiting on a held lock after 2s")
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), `"five":7,`) {
		t.Errorf("update not written while lock held: %s", b)
	}
}

// Orphans left by a killed writer age out; fresh ones (a live writer) stay.
func TestWriteUsageCacheSweepsStaleOrphans(t *testing.T) {
	path, dir := usagePath(t)
	os.MkdirAll(dir, 0o755)
	old := filepath.Join(dir, "usage.json.111")
	oldB := filepath.Join(dir, "usage.json.222")
	fresh := filepath.Join(dir, "usage.json.333")
	other := filepath.Join(dir, "cal-events.json.444")
	for _, p := range []string{old, oldB, fresh, other} {
		os.WriteFile(p, nil, 0o644)
	}
	past := time.Now().Add(-time.Hour)
	for _, p := range []string{old, oldB, other} {
		os.Chtimes(p, past, past)
	}
	writeUsageCache(path, "10", "5", "100", "6", "200")
	for _, p := range []string{old, oldB} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("stale orphan %s not removed", filepath.Base(p))
		}
	}
	for _, p := range []string{fresh, other} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed, want kept", filepath.Base(p))
		}
	}
}
