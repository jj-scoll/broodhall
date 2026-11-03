package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// queueItem is a claimed queue row joined with its captured tool use.
type queueItem struct {
	ID        int64
	SessionID string
	ToolUseID string
	ToolName  string
	Input     string
	Output    string
	Attempts  int
	DedupeKey string
}

// defaultSkipTools are the tools whose uses are never observed, matched case-insensitively.
var defaultSkipTools = map[string]bool{
	"listmcpresourcestool": true,
	"slashcommand":         true,
	"skill":                true,
	"todowrite":            true,
	"askuserquestion":      true,
}

// skipTools and skipBashPatterns are the skip lists in force; memdServe loads them from the
// settings file. The default for skip_bash_patterns is empty.
var (
	skipTools        = defaultSkipTools
	skipBashPatterns []*regexp.Regexp
)

// memdSettingsPath is memory's own settings file: $MEMORY_SETTINGS, else
// $XDG_CONFIG_HOME/memory/settings.json (default ~/.config/memory/settings.json). It is
// optional. Keys: skip_tools (comma list that replaces the default skip list), skip_bash_patterns (one
// regex) and handoff_max_age_hours (a number above zero).
func memdSettingsPath() string {
	if p := os.Getenv("MEMORY_SETTINGS"); p != "" {
		return p
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "memory", "settings.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "memory", "settings.json")
}

// loadSkipSettings reads the skip lists from the settings file: skip_tools is a comma list
// that replaces the default, and skip_bash_patterns is one regex, ignored when it does not
// compile. A missing or unparsable file, or a key that is absent or not a string, keeps the
// default.
func loadSkipSettings(path string) (map[string]bool, []*regexp.Regexp) {
	tools := defaultSkipTools
	var bash []*regexp.Regexp
	b, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("memory: skip lists: %v; using defaults", err)
		}
		return tools, bash
	}
	var s map[string]any
	if err := json.Unmarshal(b, &s); err != nil {
		log.Printf("memory: skip lists: %s: %v; using defaults", path, err)
		return tools, bash
	}
	if v, ok := s["skip_tools"].(string); ok {
		tools = map[string]bool{}
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				tools[strings.ToLower(name)] = true
			}
		}
	}
	if v, ok := s["skip_bash_patterns"].(string); ok {
		if v = strings.TrimSpace(v); v != "" {
			re, err := regexp.Compile(v)
			if err != nil {
				log.Printf("memory: skip lists: skip_bash_patterns %q ignored: %v", v, err)
			} else {
				bash = []*regexp.Regexp{re}
			}
		}
	}
	return tools, bash
}

// claimConflicts counts claim attempts retried after a DuckDB transaction conflict.
var claimConflicts int64

const claimRetries = 100

// dedupeKey is the content-addressed key for an observer call: hex sha256 of the prompt
// version, model and normalized input joined by NUL.
func dedupeKey(promptVersion, model, normalizedInput string) string {
	sum := sha256.Sum256([]byte(promptVersion + "\x00" + model + "\x00" + normalizedInput))
	return hex.EncodeToString(sum[:])
}

// shouldSkip reports whether a tool use is excluded from observation. cmdNorm is the Bash
// command with whitespace trimmed and collapsed (ruling R13).
func shouldSkip(toolName, cmdNorm string) bool {
	if skipTools[strings.ToLower(toolName)] {
		return true
	}
	if toolName == "Bash" || toolName == "PowerShell" {
		for _, re := range skipBashPatterns {
			if re.MatchString(cmdNorm) {
				return true
			}
		}
	}
	return false
}

// claimNext leases the lowest-id queued row for lease. Rows that must not reach the observer
// (skip-listed tools, or a dedupe_key already present in observations) are finished here and
// the next row is tried. It returns nil, nil when nothing is queued.
func claimNext(db *sql.DB, now time.Time, lease time.Duration) (*queueItem, error) {
	for {
		it, err := claimRow(db, now, lease)
		if err != nil || it == nil {
			return nil, err
		}
		if it.DedupeKey != "" {
			var dup bool
			if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM observations WHERE dedupe_key = ?)`, it.DedupeKey).Scan(&dup); err != nil {
				return nil, fmt.Errorf("queue dedupe check: %w", err)
			}
			if dup {
				if _, err := finish(db, it.ID, it.Attempts, "done", "dedupe"); err != nil {
					return nil, err
				}
				continue
			}
		}
		if shouldSkip(it.ToolName, bashCommand(it.ToolName, it.Input)) {
			if _, err := finish(db, it.ID, it.Attempts, "done", "skip"); err != nil {
				return nil, err
			}
			if err := recordLoss(db, "skip", it.ToolName, 1); err != nil {
				return nil, err
			}
			continue
		}
		return it, nil
	}
}

// claimRow runs the claiming UPDATE ... RETURNING in a transaction. DuckDB resolves two
// claimers racing for the same row by failing one with a conflict error; that attempt is
// retried (the row is gone from 'queued' by then). Other errors are returned.
func claimRow(db *sql.DB, now time.Time, lease time.Duration) (*queueItem, error) {
	var err error
	for i := 0; i < claimRetries; i++ {
		var it *queueItem
		it, err = claimRowOnce(db, now, lease)
		if err == nil || !isTxConflict(err) {
			return it, err
		}
		atomic.AddInt64(&claimConflicts, 1)
		time.Sleep(time.Duration(i+1) * time.Millisecond)
	}
	return nil, fmt.Errorf("queue claim: still conflicting after %d tries: %w", claimRetries, err)
}

func claimRowOnce(db *sql.DB, now time.Time, lease time.Duration) (*queueItem, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("queue claim: %w", err)
	}
	defer tx.Rollback()
	it := &queueItem{}
	err = tx.QueryRow(`UPDATE queue SET state = 'claimed', lease_until = CAST(? AS TIMESTAMP), attempts = attempts + 1
		WHERE id = (SELECT min(id) FROM queue WHERE state = 'queued')
		RETURNING id, session_id, tool_use_id, attempts, dedupe_key`, memdTS(now.Add(lease))).
		Scan(&it.ID, &it.SessionID, &it.ToolUseID, &it.Attempts, &it.DedupeKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("queue claim: %w", err)
	}
	// A missing tool_uses row leaves the fields empty; the observer decides what to do.
	err = tx.QueryRow(`SELECT tool_name, input, output FROM tool_uses WHERE session_id = ? AND tool_use_id = ?`,
		it.SessionID, it.ToolUseID).Scan(&it.ToolName, &it.Input, &it.Output)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("queue claim tool use: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("queue claim commit: %w", err)
	}
	return it, nil
}

func isTxConflict(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "conflict")
}

// bashCommand extracts the command from a Bash or PowerShell tool input and normalizes its whitespace.
func bashCommand(toolName, input string) string {
	if toolName != "Bash" && toolName != "PowerShell" {
		return ""
	}
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal([]byte(input), &in) != nil {
		return ""
	}
	return strings.Join(strings.Fields(in.Command), " ")
}

// finish sets a claimed row's outcome and clears its lease. 'queued' is allowed so the
// observer can hand a row back for retry. The update is guarded by state='claimed' and the
// attempts value the claim returned (R15): a worker whose lease expired and whose row was
// re-claimed matches nothing, and ok=false tells it to discard its result.
func finish(db *sql.DB, id int64, attempts int, state, reason string) (ok bool, err error) {
	switch state {
	case "done", "rejected", "unknown", "queued":
	default:
		return false, fmt.Errorf("queue finish: invalid state %q", state)
	}
	res, err := db.Exec(`UPDATE queue SET state = ?, reason = ?, lease_until = NULL
		WHERE id = ? AND state = 'claimed' AND attempts = ?`, state, reason, id, attempts)
	if err != nil {
		return false, fmt.Errorf("queue finish %d: %w", id, err)
	}
	return rowsAffected(res) == 1, nil
}

// reclaimExpired returns claimed rows whose lease ended before now to 'queued' and reports
// how many it moved.
func reclaimExpired(db *sql.DB, now time.Time) (int, error) {
	res, err := db.Exec(`UPDATE queue SET state = 'queued', lease_until = NULL
		WHERE state = 'claimed' AND lease_until < CAST(? AS TIMESTAMP)`, memdTS(now))
	if err != nil {
		return 0, fmt.Errorf("queue reclaim: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// recordLoss stores a loss receipt: n items lost for kind, with reason as detail.
func recordLoss(db *sql.DB, kind, reason string, n int) error {
	if _, err := db.Exec(`INSERT INTO loss_receipts (kind, detail, n) VALUES (?, ?, ?)`, kind, reason, n); err != nil {
		return fmt.Errorf("record loss %s: %w", kind, err)
	}
	return nil
}
