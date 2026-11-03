package main

// The SessionStart handoff: the briefing /handoff (or write-handoff) stored in the "handoff"
// work-state list is shown once, at the head of the next SessionStart block. Memory looks in the one project key the caller names (no
// worktree sibling projects), has no excluded-projects list, and cuts a handoff
// further when the byte guard needs room.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	handoffMaxUnits   = 3500 // the claimed block's cap, in UTF-16 units
	handoffMinCap     = 250  // the least the guard may cut a handoff block to
	handoffDefaultAge = 168 * time.Hour
	handoffCompactAge = 30 * time.Minute // a compact only re-shows a fresh handoff
	handoffHeader     = "# Handoff left by an earlier session"
)

// handoffEntry is the claimed row: its fields and when it was written.
type handoffEntry struct {
	fields  map[string]any
	created time.Time
}

// handoffClaimMu makes read-then-mark one step, so two SessionStarts cannot both show it.
var handoffClaimMu sync.Mutex

// handoffMaxAge is handoff_max_age_hours from the settings file: a finite number above zero,
// else 168 hours.
func handoffMaxAge() time.Duration {
	b, err := os.ReadFile(memdSettingsPath())
	if err != nil {
		return handoffDefaultAge
	}
	var s map[string]any
	if json.Unmarshal(b, &s) != nil {
		return handoffDefaultAge
	}
	var h float64
	switch v := s["handoff_max_age_hours"].(type) {
	case float64:
		h = v
	case string:
		h, _ = strconv.ParseFloat(strings.TrimSpace(v), 64)
	}
	if !(h > 0) || h > 24*365*100 {
		return handoffDefaultAge
	}
	return time.Duration(h * float64(time.Hour))
}

// takeHandoff finds the project's latest unshown, fresh handoff and marks it shown. Source is the
// SessionStart source: "resume" claims nothing; "compact" only re-shows one written within 30
// minutes; anything else (startup, clear, empty) takes any within the max age.
func takeHandoff(db *sql.DB, project, source string, now time.Time) (*handoffEntry, error) {
	if source == "resume" {
		return nil, nil
	}
	handoffClaimMu.Lock()
	defer handoffClaimMu.Unlock()
	rows, err := db.Query(`SELECT fields, created_at FROM work_state
		WHERE project = ? AND lower(list_name) = ? ORDER BY id`, project, wsHandoffList)
	if err != nil {
		return nil, fmt.Errorf("handoff: query: %w", err)
	}
	var all []handoffEntry
	for rows.Next() {
		var raw string
		var at time.Time
		if err := rows.Scan(&raw, &at); err != nil {
			rows.Close()
			return nil, fmt.Errorf("handoff: scan: %w", err)
		}
		var f map[string]any
		if err := json.Unmarshal([]byte(raw), &f); err != nil {
			continue // a row nothing can read is not a handoff
		}
		all = append(all, handoffEntry{f, at})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("handoff: rows: %w", err)
	}
	rows.Close()

	// The latest row that carries text is the handoff; any later consumed_at row means it was shown.
	last := len(all) - 1
	for last >= 0 && all[last].fields["text"] == nil {
		last--
	}
	if last < 0 {
		return nil, nil
	}
	for _, r := range all[last+1:] {
		if r.fields["consumed_at"] != nil {
			return nil, nil
		}
	}
	e := all[last]
	if text, ok := e.fields["text"].(string); !ok || strings.TrimSpace(text) == "" {
		return nil, nil
	}
	window := handoffMaxAge()
	if source == "compact" && handoffCompactAge < window {
		window = handoffCompactAge
	}
	if now.Sub(e.created) > window {
		return nil, nil
	}
	by := source
	if by == "" {
		by = "unknown"
	}
	if err := workStateAppend(db, project, wsHandoffList, map[string]any{"consumed_at": now.UnixMilli(), "consumed_by": by}); err != nil {
		return nil, err
	}
	return &e, nil
}

// render builds the block: header, text, details line; over max UTF-16 units the text is cut
// on whole lines (else mid-line with an ellipsis) and the details line gains "Cut to fit.".
func (e *handoffEntry) render(now time.Time, max int) string {
	head := fmt.Sprintf("%s (written %s ago, shown once)", handoffHeader, wsAgo(maxDur(0, now.Sub(e.created))))
	file := "HANDOFF.md"
	if s, ok := e.fields["handoff_file"].(string); ok && strings.TrimSpace(s) != "" {
		file = strings.TrimSpace(s)
	}
	if dir, _ := e.fields["project_dir"].(string); dir != "" && !strings.HasPrefix(file, "/") {
		file = dir + "/" + file
	}
	details := "Details: "
	if ids := wsStr(e.fields["obs_ids"]); e.fields["obs_ids"] != nil && ids != "" {
		details += "get_observations ids " + ids + "; "
	}
	details += "full text in " + file
	text := strings.TrimSpace(e.fields["text"].(string))
	if full := head + "\n" + text + "\n" + details; utf16Len(full) <= max {
		return full
	}
	cutNote := "Cut to fit. " + details
	room := max - utf16Len(head) - utf16Len(cutNote) - 2
	body := ""
	for _, ln := range strings.Split(text, "\n") {
		next := ln
		if body != "" {
			next = body + "\n" + ln
		}
		if utf16Len(next) > room {
			break
		}
		body = next
	}
	if body == "" {
		var b strings.Builder
		units := 0
		for _, r := range text {
			w := 1
			if r > 0xFFFF {
				w = 2
			}
			if units+w > room-1 {
				break
			}
			b.WriteRune(r)
			units += w
		}
		body = b.String() + "…"
	}
	return head + "\n" + body + "\n" + cutNote
}

func maxDur(a, b time.Duration) time.Duration {
	if b > a {
		return b
	}
	return a
}

// claimHandoff takes the handoff and returns its block at the full 3,500-unit cap, or "".
func claimHandoff(db *sql.DB, project, source string, now time.Time) (string, error) {
	e, err := takeHandoff(db, project, source, now)
	if e == nil || err != nil {
		return "", err
	}
	return e.render(now, handoffMaxUnits), nil
}

// buildStartContext is the SessionStart hook's block: buildContext with the claimed handoff first.
// A claim error is logged and the block goes out without one.
func buildStartContext(db *sql.DB, project, source string, now time.Time) string {
	e, err := takeHandoff(db, project, source, now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memory context: handoff: %v\n", err)
	}
	return buildContextWith(db, project, now, e)
}
