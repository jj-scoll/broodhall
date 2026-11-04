//go:build unix

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO must not hang the status line (the IsRegular check runs only after a blocking open).
func TestRemoteControlActiveFIFO(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(p, 0o644); err != nil {
		t.Skip(err)
	}
	done := make(chan bool, 1)
	go func() { done <- remoteControlActive(p) }()
	select {
	case got := <-done:
		if got {
			t.Error("FIFO reported on")
		}
	case <-time.After(2 * time.Second):
		// unblock the open so the goroutine can finish
		if w, err := os.OpenFile(p, os.O_WRONLY, 0); err == nil {
			w.Close()
		}
		t.Error("remoteControlActive blocked on a FIFO for 2s")
	}
}
