package main

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// The MCP side of raw search: search's raw mode and the read_session tool.

const (
	readSessionDefaultLimit = 40
	readSessionMaxLimit     = 200
	readSessionTextCap      = 4000 // characters of one prompt or reply
	readSessionInputCap     = 1500 // characters of one tool input
	readSessionOutputCap    = 500  // characters of one tool output
)

// rawMode reads search's source argument. Branch and model exist only on raw records, so a
// search that sets either one is a raw search.
func rawMode(args map[string]any) (raw, notesOnly bool, err error) {
	switch qStr(args, "source") {
	case "", "all":
		return qStr(args, "branch") != "" || qStr(args, "model") != "", false, nil
	case "raw":
		return true, false, nil
	case "notes":
		return false, true, nil
	}
	return false, false, fmt.Errorf(`source must be "notes" or "raw"`)
}

// memRawSearch is search over the raw session records, sessions newest first with a snippet.
// fallback marks a search that ran because the notes held nothing.
func memRawSearch(db *sql.DB, query string, f qFilter, args map[string]any, limit, offset int, fallback bool) (string, error) {
	if f.noPlatformMatch() {
		return fmt.Sprintf("No results found matching %q", query), nil
	}
	hits, err := rawSearch(db, rawQuery{
		Text: query, Project: f.project, Keys: f.keys,
		Branch: qStr(args, "branch"), Model: qStr(args, "model"),
		Start: f.start, End: f.end, Limit: limit, Offset: offset,
	})
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return fmt.Sprintf("No results found matching %q", query), nil
	}
	head := fmt.Sprintf("Found %d session(s) matching %q in the raw transcripts", len(hits), query)
	if query == "" {
		head = fmt.Sprintf("%d most recent session(s) in the raw transcripts", len(hits))
	}
	if fallback {
		head += " (no notes matched)"
	}
	out := []string{head, ""}
	for _, h := range hits {
		line := fmt.Sprintf("- `%s` %s | %s", h.SessionID, fmtDayClock(h.Last), h.Project)
		if h.Branch != "" {
			line += " | " + h.Branch
		}
		if h.Model != "" {
			line += " | " + h.Model
		}
		out = append(out, line)
		if h.Snippet != "" {
			out = append(out, "  "+h.Snippet)
		}
	}
	out = append(out, "", "Read a session with read_session (session_id).")
	return memdHC(strings.Join(out, "\n"), memdCapDefault), nil
}

// memReadSession is the read_session tool: a session's masked conversation in order.
func memReadSession(db *sql.DB, args map[string]any) (string, error) {
	id := qStr(args, "session_id")
	if id == "" {
		return "", fmt.Errorf("session_id is required")
	}
	limit, ok := qInt(args, "limit", readSessionDefaultLimit)
	if !ok {
		return "", fmt.Errorf("limit must be an integer of 0 or more")
	}
	if limit == 0 {
		limit = readSessionDefaultLimit
	}
	limit = min(limit, readSessionMaxLimit)
	offset, ok := qInt(args, "offset", 0)
	if !ok {
		return "", fmt.Errorf("offset must be an integer of 0 or more")
	}

	var project, branch, model string
	var started, last int64
	err := db.QueryRow(`SELECT project, coalesce(git_branch, ''), coalesce(model, ''), epoch_us(started_at), epoch_us(last_seen_at) FROM sessions WHERE session_id = ?`, id).
		Scan(&project, &branch, &model, &started, &last)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("no session %q", id)
	}
	if err != nil {
		return "", fmt.Errorf("read session: %w", err)
	}
	var total int
	if err := db.QueryRow(`SELECT (SELECT count(*) FROM prompts WHERE session_id = ?) + (SELECT count(*) FROM replies WHERE session_id = ?) + (SELECT count(*) FROM tool_uses WHERE session_id = ?)`, id, id, id).Scan(&total); err != nil {
		return "", fmt.Errorf("read session: %w", err)
	}

	rows, err := db.Query(fmt.Sprintf(`SELECT kind, epoch_us(ts), name, a, b, la, lb FROM (
	SELECT 'prompt' AS kind, 0 AS ord, id, created_at AS ts, '' AS name, left(text, %[1]d) AS a, '' AS b, length(text) AS la, 0 AS lb FROM prompts WHERE session_id = ?
	UNION ALL SELECT 'reply', 1, id, created_at, '', left(text, %[1]d), '', length(text), 0 FROM replies WHERE session_id = ?
	UNION ALL SELECT 'tool', 2, id, created_at, tool_name, left(input, %[2]d), left(output, %[3]d), length(input), length(output) FROM tool_uses WHERE session_id = ?
) ORDER BY ts, ord, id LIMIT ? OFFSET ?`, readSessionTextCap+1, readSessionInputCap+1, readSessionOutputCap+1), id, id, id, limit, offset)
	if err != nil {
		return "", fmt.Errorf("read session: %w", err)
	}
	defer rows.Close()

	var body []string
	n := 0
	for rows.Next() {
		var kind, name, a, b string
		var ts int64
		var la, lb int
		if err := rows.Scan(&kind, &ts, &name, &a, &b, &la, &lb); err != nil {
			return "", err
		}
		n++
		at := fmtClock(time.UnixMicro(ts).UTC())
		switch kind {
		case "prompt":
			body = append(body, fmt.Sprintf("[%d] %s you:", offset+n, at), readSessionCut(a, la, readSessionTextCap), "")
		case "reply":
			body = append(body, fmt.Sprintf("[%d] %s claude:", offset+n, at), readSessionCut(a, la, readSessionTextCap), "")
		default:
			body = append(body, fmt.Sprintf("[%d] %s tool %s:", offset+n, at, name),
				"input: "+readSessionCut(a, la, readSessionInputCap),
				"output: "+readSessionCut(b, lb, readSessionOutputCap), "")
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	head := fmt.Sprintf("Session %s | %s", id, project)
	if branch != "" {
		head += " | " + branch
	}
	if model != "" {
		head += " | " + model
	}
	head += fmt.Sprintf(" | %s to %s", fmtDayClock(time.UnixMicro(started).UTC()), fmtDayClock(time.UnixMicro(last).UTC()))
	var span string
	switch {
	case n == 0:
		span = fmt.Sprintf("No items at offset %d (the session has %d).", offset, total)
	case offset+n < total:
		span = fmt.Sprintf("Items %d-%d of %d. More: offset=%d.", offset+1, offset+n, total, offset+n)
	default:
		span = fmt.Sprintf("Items %d-%d of %d.", offset+1, offset+n, total)
	}
	out := append([]string{head, span, ""}, body...)
	// Stored text is masked when it is written; masking again costs little and covers rows
	// written by an older version.
	return memdHC(memdRedact(strings.Join(out, "\n")), memdCapDefault), nil
}

// readSessionCut marks text that the query cut short; total is its full length in characters.
func readSessionCut(s string, total, limit int) string {
	if total <= limit {
		return s
	}
	return clipRunes(s, limit) + fmt.Sprintf("... (%d characters in all)", total)
}
