package main

// memory session summaries: one summary per burst of turns. The tailer arms a summary_requests
// row on turn_end (R29; summaryDebounce after the first turn_end of a burst); runSummaryWorker picks due rows and
// summarize makes one attempt. Outcomes mirror the observer: done (append a summaries row),
// rejected (valid JSON that fails validation twice; never defaulted, with a loss receipt),
// unknown (auth/transport/CLI failure: stays pending with a backoff so an outage loses
// nothing; cli_error and missing_structured_output are capped like R25, auth and transport
// are not). summaries is fts-indexed and append-only, so a summary is only ever inserted.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// summaryDebounce is the delay from the first turn_end of a burst to its summary being due
// (leading-edge: later turn_ends do not move it).
// Var so tests need no sleeps.
var summaryDebounce = 60 * time.Second

// summaryTag matches the opening or closing delimiter tags of the payload, with optional
// whitespace, case-insensitively, so session text cannot close or reopen the data block.
var summaryTag = regexp.MustCompile(`(?i)<\s*(/?)\s*(session_data|prompt|observation)\b`)

func escapeSummary(s string) string { return summaryTag.ReplaceAllString(s, `<\${1}${2}`) }

// summaryText is a validated summary; every field is present.
type summaryText struct{ f map[string]string }

// parseSummary decodes and validates structured output. reason is "" or a code: invalid_shape
// (not an object of strings), missing_field, <field>_too_long, empty_summary (all blank).
func parseSummary(so json.RawMessage) (*summaryText, string) {
	var raw map[string]*string
	if json.Unmarshal(so, &raw) != nil {
		return nil, "invalid_shape"
	}
	s := &summaryText{f: map[string]string{}}
	blank := true
	for _, name := range summaryFields {
		v := raw[name]
		if v == nil {
			return nil, "missing_field"
		}
		if utf8.RuneCountInString(*v) > summaryFieldMax {
			return nil, name + "_too_long"
		}
		blank = blank && strings.TrimSpace(*v) == ""
		s.f[name] = *v
	}
	if blank {
		return nil, "empty_summary"
	}
	return s, ""
}

// summaryPayload is the data-delimited stdin body: the session's most recent prompts and
// observation titles, oldest first. Each is redacted, then cut, then escaped. empty is true
// when the session has neither.
func summaryPayload(db *sql.DB, sessionID string) (payload string, empty bool, err error) {
	collect := func(q string, max int, tag string, cut func(string) string) ([]string, error) {
		rows, err := db.Query(q, sessionID, max)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return nil, err
			}
			out = append(out, "<"+tag+">"+escapeSummary(cut(memdRedact(s)))+"</"+tag+">\n")
		}
		return out, rows.Err()
	}
	prompts, err := collect(`SELECT text FROM (SELECT text, created_at, id FROM prompts WHERE session_id = ?
		ORDER BY created_at DESC, id DESC LIMIT ?) ORDER BY created_at, id`, summaryMaxPrompts, "prompt",
		func(s string) string { c, _ := capPayload(s, summaryPromptHead, summaryPromptTail); return c })
	if err != nil {
		return "", false, fmt.Errorf("summary prompts: %w", err)
	}
	titles, err := collect(`SELECT title FROM (SELECT title, created_at, id FROM observations WHERE session_id = ?
		ORDER BY created_at DESC, id DESC LIMIT ?) ORDER BY created_at, id`, summaryMaxTitles, "observation",
		func(s string) string { c, _ := capPayload(s, summaryTitleMax, 0); return c })
	if err != nil {
		return "", false, fmt.Errorf("summary observations: %w", err)
	}
	if len(prompts)+len(titles) == 0 {
		return "", true, nil
	}
	return "<session_data>\n" + strings.Join(prompts, "") + strings.Join(titles, "") + "</session_data>\n", false, nil
}

// summarizeCall runs the model: validation failures get exactly one retry whose stdin
// carries the reason. outcome is done, rejected or unknown (reason is then a class constant).
func summarizeCall(ctx context.Context, payload string) (s *summaryText, outcome, reason string) {
	argv := observerArgv(summarySystemPrompt, summarySchema())
	stdin := summaryStdinHeader + payload
	for attempt := 0; attempt < 2; attempt++ {
		so, why, _, ok := structuredOnce(ctx, stdin, argv)
		if !ok {
			return nil, "unknown", why
		}
		s, why = parseSummary(so)
		if why == "" {
			return s, "done", ""
		}
		reason = why
		stdin = summaryStdinHeader + payload +
			"\nYour previous reply failed validation (" + why + "). Reply again with a corrected object.\n"
	}
	return nil, "rejected", reason
}

// summarize makes one attempt for sessionID's pending request and records the outcome on the
// request. It returns an error only for database failures or cancellation (nothing is
// written then); a model failure is an outcome, not an error. A request that is not pending
// is left alone.
func summarize(ctx context.Context, db *sql.DB, sessionID string) error {
	var attempts, gen int
	err := db.QueryRow(`SELECT attempts, gen FROM summary_requests WHERE session_id = ? AND state = 'pending'`, sessionID).Scan(&attempts, &gen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read summary request: %w", err)
	}
	payload, empty, err := summaryPayload(db, sessionID)
	if err != nil {
		return err
	}
	if empty {
		return withWrite(func() error {
			_, err := settleSummary(db, sessionID, attempts, gen, "done", "empty", nil)
			return err
		})
	}
	s, outcome, reason := summarizeCall(ctx, payload)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return withWrite(func() error { return recordSummary(db, sessionID, attempts, gen, s, outcome, reason) })
}

// Settle results. settleLost: the request was no longer pending at the attempt's attempts.
const (
	settleLost    = iota
	settleApplied // the requested state was written
	settleRearmed // gen moved (a turn_end arrived mid-attempt): kept pending, see settleSummary
)

// settleSummary is the guarded request update: the request must still be pending at the
// attempts and gen the attempt started from. If gen moved (R32) a turn_end arrived after the
// attempt read the session, so whatever the outcome the request stays pending with
// attempts = 0 and due_at = now + summaryDebounce, and the result is settleRearmed. due, when
// set, is the retry time for an unknown outcome (attempts + 1).
func settleSummary(ex execer, sessionID string, attempts, gen int, state, reason string, due *time.Time) (int, error) {
	var dueS any
	newAttempts := attempts
	if due != nil {
		dueS, newAttempts = memdTS(*due), attempts+1
	}
	res, err := ex.Exec(`UPDATE summary_requests SET state = ?, reason = ?, attempts = ?,
		due_at = coalesce(CAST(? AS TIMESTAMP), due_at), updated_at = current_timestamp
		WHERE session_id = ? AND state = 'pending' AND attempts = ? AND gen = ?`,
		state, reason, newAttempts, dueS, sessionID, attempts, gen)
	if err != nil {
		return settleLost, fmt.Errorf("settle summary request: %w", err)
	}
	if rowsAffected(res) == 1 {
		return settleApplied, nil
	}
	res, err = ex.Exec(`UPDATE summary_requests SET attempts = 0, reason = '',
		due_at = CAST(? AS TIMESTAMP), updated_at = current_timestamp
		WHERE session_id = ? AND state = 'pending' AND attempts = ? AND gen <> ?`,
		memdTS(memdNow().Add(summaryDebounce)), sessionID, attempts, gen)
	if err != nil {
		return settleLost, fmt.Errorf("rearm summary request: %w", err)
	}
	if rowsAffected(res) == 1 {
		return settleRearmed, nil
	}
	return settleLost, nil
}

// recordSummary writes one outcome (caller holds memdWriteMu). Loss receipts are written
// only when the rejection was applied.
func recordSummary(db *sql.DB, sessionID string, attempts, gen int, s *summaryText, outcome, reason string) error {
	switch outcome {
	case "done":
		return storeSummary(db, sessionID, attempts, gen, s)
	case "rejected":
		r, err := settleSummary(db, sessionID, attempts, gen, "rejected", reason, nil)
		if err != nil || r != settleApplied {
			return err
		}
		return recordLoss(db, "summary_rejected", reason, 1)
	}
	class := reasonClass(reason)
	if (class == reasonCLIError || class == reasonNoStructure) && attempts+1 >= observerMaxAttempts {
		r, err := settleSummary(db, sessionID, attempts, gen, "rejected", "attempts_exhausted", nil)
		if err != nil || r != settleApplied {
			return err
		}
		return recordLoss(db, "summary_rejected", "attempts_exhausted", 1)
	}
	due := memdNow().Add(observerBackoff(attempts + 1))
	_, err := settleSummary(db, sessionID, attempts, gen, "pending", reason, &due)
	return err
}

// storeSummary inserts the summary and settles the request in one transaction; a request
// that is no longer pending rolls the insert back. A moved gen keeps the summary (it is
// valid for what it read) and leaves the request pending.
func storeSummary(db *sql.DB, sessionID string, attempts, gen int, s *summaryText) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store summary: %w", err)
	}
	defer tx.Rollback()
	var project string
	err = tx.QueryRow(`SELECT project FROM sessions WHERE session_id = ?`, sessionID).Scan(&project)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store summary project: %w", err)
	}
	var sid int64
	err = tx.QueryRow(`INSERT INTO summaries (project, session_id, request, investigated, learned, completed,
		next_steps, notes, prompt_version, generated_by_model, source) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'live') RETURNING id`,
		project, sessionID, s.f["request"], s.f["investigated"], s.f["learned"], s.f["completed"],
		s.f["next_steps"], s.f["notes"], summaryPromptVersion, observerModel).Scan(&sid)
	if err != nil {
		return fmt.Errorf("insert summary: %w", err)
	}
	r, err := settleSummary(tx, sessionID, attempts, gen, "done", "", nil)
	if err != nil || r == settleLost {
		return err // deferred Rollback discards the insert
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store summary commit: %w", err)
	}
	memdIndexSummary(db, sid)
	return nil
}

// runSummaryWorker summarizes due pending requests, one at a time, until ctx is cancelled.
func runSummaryWorker(ctx context.Context, db *sql.DB) {
	for ctx.Err() == nil {
		var sid string
		err := db.QueryRow(`SELECT session_id FROM summary_requests
			WHERE state = 'pending' AND due_at <= CAST(? AS TIMESTAMP) ORDER BY due_at, session_id LIMIT 1`,
			memdTS(memdNow())).Scan(&sid)
		if err == nil {
			if err = summarize(ctx, db, sid); err == nil {
				continue
			}
		}
		if !errors.Is(err, sql.ErrNoRows) && ctx.Err() == nil {
			workerLogf("memory summary: %v", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(observerPoll):
		}
	}
}
