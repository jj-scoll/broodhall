package main

// The guard ledger: each guard appends one line per tool call it masks, asks about, blocks or
// nudges, so the receipt Stop hook can tell the user what the guards did this turn (everything
// else they say reaches only Claude). A line holds the time, the kind and the rule id: never
// command text, paths or values, so the ledger cannot leak what a guard masked. Subagents
// share the session id, so their events land in the same ledger. A failed write is ignored:
// the ledger must never change or slow a guard's decision.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type guardEvent struct{ Kind, Rule string }

func guardLedger(session string) string { return filepath.Join(stateDir(), session+".events") }

// logGuardEvent records one event. kind is mask, ask, block or nudge.
func logGuardEvent(session, kind, rule string) {
	if !safeSession.MatchString(session) {
		return
	}
	rule, _, _ = strings.Cut(rule, " ")
	if os.MkdirAll(stateDir(), 0o755) != nil {
		return
	}
	// One small O_APPEND write per event: concurrent hooks don't interleave lines.
	f, err := os.OpenFile(guardLedger(session), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	fmt.Fprintf(f, "%d\t%s\t%s\n", time.Now().Unix(), kind, rule)
	f.Close()
}

// guardSummary counts the session's whole ledger for the statusline, e.g. "guards 2 masked ·
// 1 asked", or "" when there are no events. It only reads: the .off offset belongs to the
// receipt Stop hook (newGuardEvents).
func guardSummary(session string) string {
	if !safeSession.MatchString(session) {
		return ""
	}
	data, err := os.ReadFile(guardLedger(session))
	if err != nil {
		return ""
	}
	n := map[string]int{}
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Split(line, "\t"); len(f) == 3 {
			n[f[1]]++
		}
	}
	var parts []string
	for _, k := range []struct{ kind, word string }{{"mask", "masked"}, {"ask", "asked"}, {"block", "blocked"}, {"nudge", "nudged"}} {
		if n[k.kind] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n[k.kind], k.word))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "guards " + strings.Join(parts, " · ")
}

// newGuardEvents returns the events logged since the last commit. commit marks them seen; a
// caller that doesn't commit gets them again next time.
func newGuardEvents(session string) (evs []guardEvent, commit func()) {
	commit = func() {}
	if !safeSession.MatchString(session) {
		return nil, commit
	}
	data, err := os.ReadFile(guardLedger(session))
	if err != nil {
		return nil, commit
	}
	offFile := guardLedger(session) + ".off"
	b, _ := os.ReadFile(offFile)
	off, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || off < 0 || off > len(data) {
		off = 0 // no offset yet, or the ledger was replaced
	}
	end := off + bytes.LastIndexByte(data[off:], '\n') + 1 // a line still being written waits
	for _, line := range strings.Split(string(data[off:end]), "\n") {
		if f := strings.Split(line, "\t"); len(f) == 3 {
			evs = append(evs, guardEvent{f[1], f[2]})
		}
	}
	return evs, func() { os.WriteFile(offFile, []byte(strconv.Itoa(end)), 0o600) }
}
