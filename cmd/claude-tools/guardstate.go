package main

// fileState is the guardState the hooks use: one small file per session in the handoff
// state dir, one "epoch<TAB>key" line per mark. Reads and writes are best effort: a state
// failure must never block a tool call, it only costs a repeated nudge.

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	safeSession = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	digits      = regexp.MustCompile(`^[0-9]+$`)
)

// stateDir is the handoff state dir the hooks share: $HANDOFF_STATE_DIR/claude-handoff.
func stateDir() string {
	base := os.Getenv("HANDOFF_STATE_DIR")
	if base == "" {
		base = "/tmp"
	}
	return filepath.Join(base, "claude-handoff")
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

type fileState struct{ path string }

// newFileState returns nil for a session id that is not safe to use in a file name.
func newFileState(session string) guardState {
	if !safeSession.MatchString(session) {
		return nil
	}
	return &fileState{filepath.Join(stateDir(), session+".guard")}
}

func cleanKey(k string) string { return strings.NewReplacer("\n", " ", "\t", " ").Replace(k) }

func (f *fileState) Seen(key string, within time.Duration, now time.Time) bool {
	b, err := os.ReadFile(f.path)
	if err != nil {
		return false
	}
	key = cleanKey(key)
	for _, line := range strings.Split(string(b), "\n") {
		es, k, ok := strings.Cut(line, "\t")
		if !ok || k != key {
			continue
		}
		if e, err := strconv.ParseInt(es, 10, 64); err == nil && now.Sub(time.Unix(e, 0)) <= within {
			return true
		}
	}
	return false
}

func (f *fileState) Mark(key string, now time.Time) {
	os.MkdirAll(filepath.Dir(f.path), 0o755)
	if st, err := os.Stat(f.path); err == nil && st.Size() > 32<<10 {
		f.prune(now)
	}
	fh, err := os.OpenFile(f.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer fh.Close()
	fh.WriteString(strconv.FormatInt(now.Unix(), 10) + "\t" + cleanKey(key) + "\n")
}

// prune drops marks older than a day.
func (f *fileState) prune(now time.Time) {
	b, err := os.ReadFile(f.path)
	if err != nil {
		return
	}
	var kept strings.Builder
	for _, line := range strings.Split(string(b), "\n") {
		es, _, ok := strings.Cut(line, "\t")
		if e, err := strconv.ParseInt(es, 10, 64); ok && err == nil && now.Sub(time.Unix(e, 0)) <= 24*time.Hour {
			kept.WriteString(line + "\n")
		}
	}
	os.WriteFile(f.path, []byte(kept.String()), 0o644)
}
