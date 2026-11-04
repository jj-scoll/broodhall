package main

import (
	"os"
	"testing"
)

// sockDirBudget is the longest directory path a test socket may live under. macOS limits a
// unix socket path (sun_path) to 104 bytes, Linux to 108; the longest socket name the tests
// bind is "/memory.sock" (12 bytes), so this leaves headroom under the lower limit.
// Same helper as cmd/memory/sockdir_test.go.
const sockDirBudget = 80

// shortSockDir returns a fresh directory whose path is short enough to hold a unix socket.
// t.TempDir() cannot be used for this: on macOS it lives under /var/folders/xx/<hash>/T/
// and embeds the test name, which pushes socket paths past 104 bytes.
func shortSockDir(t *testing.T) string {
	t.Helper()
	base := os.TempDir()
	if len(base) > sockDirBudget-12 {
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "m")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if len(dir) > sockDirBudget {
		t.Fatalf("socket dir %q is %d bytes, over the %d budget", dir, len(dir), sockDirBudget)
	}
	return dir
}
