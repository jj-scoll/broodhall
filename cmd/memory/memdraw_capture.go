package main

import (
	"database/sql"
	"fmt"
	"strings"
)

// Capture side of raw search: Claude's reply text and the session's branch and model.

// replyEvent turns one text block of an assistant line into a "reply" event. The key is the
// line's uuid (else the message id) plus the block index, so reading a transcript again
// yields the same key. Blank blocks, and lines with neither id, give no event; thinking
// blocks never reach here.
func replyEvent(base memdEvent, line, msg *obj, block int, b *obj, st *memdFileState) (memdEvent, bool) {
	text := strings.TrimSpace(b.getStr("text"))
	if text == "" {
		return memdEvent{}, false
	}
	id := line.getStr("uuid")
	if id == "" {
		id = msg.getStr("id")
	}
	if id == "" {
		return memdEvent{}, false
	}
	e := base
	e.Kind = "reply"
	e.ReplyKey = fmt.Sprintf("%s#%d", id, block)
	e.ReplyText = memdRedact(text)
	e.Observer, e.Sub = st.Observer, st.Sub
	return e, true
}

// insertReply stores a reply unless it is already there and returns 1 when it inserted. An
// observer's reply is not kept, and a subagent's is not either: it would interleave with
// the main conversation, as a subagent's prompt would.
func insertReply(tx *sql.Tx, e memdEvent, at string) (int, error) {
	if e.Sub || e.Observer {
		return 0, nil
	}
	res, err := tx.Exec(`INSERT INTO replies (session_id, reply_key, text, created_at) VALUES (?, ?, ?, CAST(? AS TIMESTAMP))
ON CONFLICT (session_id, reply_key) DO NOTHING`, e.SessionID, e.ReplyKey, e.ReplyText, at)
	if err != nil {
		return 0, fmt.Errorf("insert reply: %w", err)
	}
	return rowsAffected(res), nil
}

// touchSessionMeta records the branch and model the event carries; an empty value keeps the
// last one seen. A subagent's model is not the session's, so its events change neither.
func touchSessionMeta(tx *sql.Tx, e memdEvent) error {
	if e.Sub || (e.GitBranch == "" && e.Model == "") {
		return nil
	}
	if _, err := tx.Exec(`UPDATE sessions SET git_branch = coalesce(nullif(?, ''), git_branch), model = coalesce(nullif(?, ''), model) WHERE session_id = ?`,
		e.GitBranch, e.Model, e.SessionID); err != nil {
		return fmt.Errorf("update session branch and model: %w", err)
	}
	return nil
}
