package main

// handoffSegment is the statusline's view of the auto-handoff chain (gate -> autoclear ->
// write-handoff -> context-guard) for one session. It stats a few files in the handoff state
// dir and reads at most one tiny one; it never touches the transcript. See
// docs/guardrails/handoff-gate.md for the files.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	handoffWaitTTL    = 600 // seconds the gate's mark counts as "waiting for the next-steps file"
	handoffWriterTTL  = 900 // a .writing marker older than this belongs to a writer that died
	handoffOutcomeTTL = 120 // seconds a cleared or failed outcome stays visible
	handoffWrittenTTL = 600 // seconds "written, run /clear" stays visible (the user must act)
	handoffColWork    = 117 // in progress
	handoffColWait    = 214 // waiting on context-guard or on you
	handoffColDone    = 82  // cleared
	handoffColFail    = 196 // failed
)

// handoffSegment returns the segment text and its 256-colour code, or "" when nothing is
// happening for the session.
func handoffSegment(state, session string, now int64) (string, int) {
	if !safeSession.MatchString(session) {
		return "", 0
	}
	path := func(suffix string) string { return filepath.Join(state, session+"."+suffix) }
	age := func(suffix string) (int64, bool) {
		st, err := os.Stat(path(suffix))
		if err != nil {
			return 0, false
		}
		return now - st.ModTime().Unix(), true
	}
	first := func(suffix string) string { // the handoff file's base name, from line 1
		b, err := os.ReadFile(path(suffix))
		if err != nil {
			return ""
		}
		line, _, _ := strings.Cut(string(b), "\n")
		return filepath.Base(line)
	}

	if _, ok := age("claimed"); ok {
		return "handoff: clearing", handoffColWork
	}
	if _, ok := age("rearm"); ok {
		return "handoff: clear armed", handoffColWait
	}
	if a, ok := age("writing"); ok && a <= handoffWriterTTL {
		if b, err := os.ReadFile(path("writing")); err == nil {
			if start, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil && now >= start {
				return "handoff: writing " + fmtElapsed(now-start), handoffColWork
			}
		}
		return "handoff: writing", handoffColWork
	}
	// Gated and waiting for the next-steps file: the gate's mark is recent and no writer has
	// started since it.
	if g, ok := age("gate"); ok && g <= handoffWaitTTL {
		if a, started := age("autoclear"); !started || a > g {
			return "handoff: next-steps", handoffColWork
		}
	}
	if a, ok := age("failed"); ok && a <= handoffOutcomeTTL {
		return "handoff: failed (see writer.log)", handoffColFail
	}
	if a, ok := age("cleared"); ok && a <= handoffOutcomeTTL {
		return "handoff: cleared → " + first("cleared"), handoffColDone
	}
	if a, ok := age("written"); ok && a <= handoffWrittenTTL {
		return "handoff: written → " + first("written") + ", run /clear", handoffColWait
	}
	return "", 0
}

// fmtElapsed renders seconds as 23s or 1m30s.
func fmtElapsed(s int64) string {
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	return fmt.Sprintf("%dm%02ds", s/60, s%60)
}
