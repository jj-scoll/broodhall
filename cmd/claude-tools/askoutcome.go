package main

// Ask outcomes: whether an ASK was approved. PreToolUse payloads and the PostToolUse and
// PostToolUseFailure payloads of the same call carry the same tool_use_id (PermissionRequest
// has none, and PermissionDenied does not fire for a manual deny). So an ask leaves a pending
// file <session>.ask.<tool_use_id> holding its rule id, and a later PostToolUse or
// PostToolUseFailure with the same session and id consumes it and logs "ran": the user
// approved. An ask still pending at the session's Stop (receipt hook), or older than
// askGrace at guard-report time, is logged "not-run". A denial, an interrupt at the prompt
// and an abandoned session look the same from here, so the report says "not run", not
// "denied".
//
// One file per ask means the PostToolUse path with no pending ask is one failed open(2),
// no ledger read, and concurrent hooks (subagents share the session) never rewrite a shared
// file: whoever removes the file first logs the outcome, once. Observe-mode would-asks never
// come through recordAsk, so they are never pending and never counted as approved. A ledger
// write that fails is ignored like every ledger write.

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// askGrace is how long guard-report waits before it calls a pending ask not run, so a prompt
// the user is looking at right now is not counted.
const askGrace = 10 * time.Minute

var safeToolUseID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func askFile(session, id string) string { return filepath.Join(stateDir(), session+".ask."+id) }

// recordAsk logs an ask and, when the call has a usable id, leaves it pending.
func recordAsk(session, toolUseID, rule string) {
	logGuardEvent(session, "ask", rule)
	recordAskPending(session, toolUseID, rule)
}

func recordAskPending(session, id, rule string) {
	if !safeSession.MatchString(session) || !safeToolUseID.MatchString(id) {
		return
	}
	rule, _, _ = strings.Cut(rule, " ")
	if os.MkdirAll(stateDir(), 0o755) != nil {
		return
	}
	os.WriteFile(askFile(session, id), []byte(rule+"\n"), 0o600)
}

// ruleOfPending turns a pending file's content into a rule id. Content that is empty, binary
// or unreadable still stands for an ask and reads as "unknown".
func ruleOfPending(b []byte) string {
	r, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	r, _, _ = strings.Cut(r, " ")
	if r == "" || len(r) > 100 || strings.ContainsFunc(r, func(c rune) bool { return c < ' ' || c > '~' }) {
		return "unknown"
	}
	return r
}

// consumeAsk removes one pending file and logs the outcome, exactly once even when two hooks
// race for it: only the caller whose remove succeeds logs.
func consumeAsk(session, path, kind string) {
	// Only a regular file counts: a symlink would put another file's text in the ledger.
	if st, err := os.Lstat(path); err != nil || !st.Mode().IsRegular() {
		return
	}
	b, _ := os.ReadFile(path)
	if os.Remove(path) != nil {
		return
	}
	logGuardEvent(session, kind, ruleOfPending(b))
}

func askOutcome(args []string) int {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return 0
	}
	return askOutcomeRun(data)
}

// askOutcomeRun is the PostToolUse/PostToolUseFailure hook. With no pending ask for the call
// it costs one failed open and writes nothing. Never blocks, never prints.
func askOutcomeRun(data []byte) int {
	var in struct {
		Event     string `json:"hook_event_name"`
		SessionID string `json:"session_id"`
		ToolUseID string `json:"tool_use_id"`
	}
	if json.Unmarshal(data, &in) != nil || !safeSession.MatchString(in.SessionID) || !safeToolUseID.MatchString(in.ToolUseID) {
		return 0
	}
	if in.Event != "PostToolUse" && in.Event != "PostToolUseFailure" {
		return 0
	}
	path := askFile(in.SessionID, in.ToolUseID)
	if _, err := os.Lstat(path); err != nil { // the whole cost when nothing is pending
		return 0
	}
	consumeAsk(in.SessionID, path, "ran")
	return 0
}

// resolveAsks logs "not-run" for the pending asks of one session (all sessions when session
// is "") that are at least olderThan old, and removes them.
func resolveAsks(session string, olderThan time.Duration) {
	pat := "*"
	if session != "" {
		if !safeSession.MatchString(session) {
			return
		}
		pat = session
	}
	files, _ := filepath.Glob(filepath.Join(stateDir(), pat+".ask.*"))
	for _, f := range files {
		sess, id, ok := strings.Cut(filepath.Base(f), ".ask.")
		if !ok || !safeSession.MatchString(sess) || !safeToolUseID.MatchString(id) {
			continue
		}
		if olderThan > 0 {
			st, err := os.Lstat(f)
			if err != nil || time.Since(st.ModTime()) < olderThan {
				continue
			}
		}
		consumeAsk(sess, f, "not-run")
	}
}
