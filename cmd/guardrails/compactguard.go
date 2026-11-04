package main

// compact-guard (PreCompact hook): block the first manual /compact once a session has
// already compacted (manual or auto), pointing at the memory plugin's handoff skill and
// /clear instead. Running /compact again goes through. Auto-compact is never blocked:
// blocking one that recovers from a context-limit error fails the request.
//
// Each compaction also re-arms gate (removes its once-per-session mark), so a session that
// keeps going writes a fresh handoff before its next compaction. It also deletes the
// session's read-dedup ledger, since the compacted conversation no longer holds those reads.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func compactGuard(args []string) int {
	var in struct {
		SessionID string `json:"session_id"`
		Trigger   string `json:"trigger"`
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil || json.Unmarshal(data, &in) != nil || !safeSession.MatchString(in.SessionID) {
		return 0
	}
	state := stateDir()
	countFile := filepath.Join(state, in.SessionID+".compacts")
	blocked := filepath.Join(state, in.SessionID+".compact-blocked")
	os.MkdirAll(state, 0o755)

	// Anything but digits counts as 0.
	var count int64
	if b, err := os.ReadFile(countFile); err == nil {
		if s := strings.TrimRight(string(b), "\n"); digits.MatchString(s) {
			count, _ = strconv.ParseInt(s, 10, 64)
		}
	}

	if in.Trigger == "manual" && count >= 1 && !exists(blocked) {
		touch(blocked)
		fmt.Fprintln(os.Stderr, "This session has already compacted once; compacting again degrades context further. Run /memory:handoff, then /clear (the new session shows the handoff at its start). Run /compact again to compact anyway.")
		return 2
	}

	os.WriteFile(countFile, []byte(strconv.FormatInt(count+1, 10)+"\n"), 0o644)
	os.Remove(filepath.Join(state, in.SessionID+".gate"))
	// Claude is about to lose its earlier reads, so read-dedup must not claim them.
	os.Remove(readCachePath(in.SessionID))
	return 0
}
