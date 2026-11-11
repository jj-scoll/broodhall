package main

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// rawQuery is a raw search: the Atuin-style text plus session filters. Keys holds the project
// and its aliases (projectKeys); it falls back to Project.
type rawQuery struct {
	Text          string
	Project       string
	Keys          []string
	Branch, Model string // Branch is an exact match; Model matches as a case-insensitive substring
	Start, End    *time.Time
	Limit, Offset int
}

// rawHit is one matching session. Snippet is a short excerpt around the match, or for an
// empty query the session's first prompt.
type rawHit struct {
	SessionID, Project, Branch, Model string
	Started, Last                     time.Time
	Snippet                           string
}

const (
	rawSnippetBefore = 70
	rawSnippetAfter  = 110
)

// rawSearch returns the sessions matching q, most recently active first. Observer sessions
// are never returned. A session matches when every word and phrase of the query occurs in
// it, possibly in different messages; a phrase must sit inside one message.
func rawSearch(db *sql.DB, q rawQuery) ([]rawHit, error) {
	if err := rawCatchUp(db); err != nil {
		return nil, fmt.Errorf("raw search index: %w", err)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = rawDefaultList
	}
	conds := []string{"NOT s.is_observer"}
	var args []any
	units := parseRawQuery(q.Text)
	ids := map[string]int{}
	if len(units) > 0 {
		var ok bool
		var err error
		if ids, ok, err = rawTermIDs(db, units); err != nil || !ok {
			return nil, err // a word that was never indexed matches nothing
		}
		for _, u := range units {
			conds = append(conds, "s.session_id IN (SELECT d.session_id FROM "+rawUnitFrom(u, ids, true)+")")
		}
	}
	if q.Project != "" || len(q.Keys) > 0 {
		keys := q.Keys
		if len(keys) == 0 {
			keys = []string{q.Project}
		}
		pm, pa := projectMatch("s.project", keys)
		conds = append(conds, pm)
		args = append(args, pa...)
	}
	if q.Branch != "" {
		conds = append(conds, "s.git_branch = ?")
		args = append(args, q.Branch)
	}
	if q.Model != "" {
		conds = append(conds, `lower(s.model) LIKE ? ESCAPE '\'`)
		args = append(args, "%"+likeEscape(strings.ToLower(q.Model))+"%")
	}
	if q.Start != nil { // sessions active in the range
		conds = append(conds, "s.last_seen_at >= CAST(? AS TIMESTAMP)")
		args = append(args, memdTS(*q.Start))
	}
	if q.End != nil {
		conds = append(conds, "s.started_at <= CAST(? AS TIMESTAMP)")
		args = append(args, memdTS(*q.End))
	}
	args = append(args, limit, max(q.Offset, 0))
	rows, err := db.Query(`SELECT s.session_id, s.project, coalesce(s.git_branch, ''), coalesce(s.model, ''), epoch_us(s.started_at), epoch_us(s.last_seen_at)
FROM sessions s WHERE `+strings.Join(conds, " AND ")+` ORDER BY s.last_seen_at DESC, s.session_id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("raw search: %w", err)
	}
	var hits []rawHit
	for rows.Next() {
		var h rawHit
		var started, last int64
		if err := rows.Scan(&h.SessionID, &h.Project, &h.Branch, &h.Model, &started, &last); err != nil {
			rows.Close()
			return nil, err
		}
		h.Started, h.Last = time.UnixMicro(started).UTC(), time.UnixMicro(last).UTC()
		hits = append(hits, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range hits {
		hits[i].Snippet = rawSnippet(db, hits[i].SessionID, units, ids)
	}
	return hits, nil
}

// rawTermIDs resolves the query's terms to dictionary ids; ok is false when one is missing.
func rawTermIDs(db *sql.DB, units []rawUnit) (map[string]int, bool, error) {
	set := map[string]bool{}
	var args []any
	for _, u := range units {
		for _, t := range u {
			if !set[t] {
				set[t] = true
				args = append(args, t)
			}
		}
	}
	// Queries are short; a very long one is cut rather than failing on the parameter count.
	if len(args) > 200 {
		return nil, false, fmt.Errorf("query has too many distinct words (%d, the limit is 200)", len(args))
	}
	rows, err := db.Query(`SELECT term, termid FROM rx_dict WHERE term IN (`+strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")+`)`, args...)
	if err != nil {
		return nil, false, fmt.Errorf("raw search terms: %w", err)
	}
	defer rows.Close()
	ids := make(map[string]int, len(args))
	for rows.Next() {
		var t string
		var id int
		if err := rows.Scan(&t, &id); err != nil {
			return nil, false, err
		}
		ids[t] = id
	}
	return ids, len(ids) == len(set), rows.Err()
}

// rawUnitFrom is the FROM clause (with its WHERE) that yields, as p0.doc, every doc holding
// the unit: its terms at consecutive positions. Term ids are integers from rx_dict. With
// docs, rx_docs is joined in as d.
func rawUnitFrom(u rawUnit, ids map[string]int, docs bool) string {
	var b strings.Builder
	b.WriteString("rx_post p0")
	for i := 1; i < len(u); i++ {
		fmt.Fprintf(&b, " JOIN rx_post p%[1]d ON p%[1]d.doc = p0.doc AND p%[1]d.pos = p0.pos + %[1]d AND p%[1]d.termid = %[2]d", i, ids[u[i]])
	}
	if docs {
		b.WriteString(" JOIN rx_docs d ON d.id = p0.doc")
	}
	fmt.Fprintf(&b, " WHERE p0.termid = %d", ids[u[0]])
	return b.String()
}

// rawSnippet is a short excerpt for one hit: around the first unit's most recent match, or
// the session's first prompt when the query is empty. Failure yields no snippet; the hit
// stands without it.
func rawSnippet(db *sql.DB, session string, units []rawUnit, ids map[string]int) string {
	if len(units) == 0 {
		var text string
		if db.QueryRow(`SELECT text FROM prompts WHERE session_id = ? ORDER BY created_at, id LIMIT 1`, session).Scan(&text) != nil {
			return ""
		}
		return "[prompt] " + rawExcerpt(text, "", 0, 140)
	}
	var kind string
	var src int64
	err := db.QueryRow(`SELECT d.kind, d.src_id FROM rx_docs d WHERE d.session_id = ? AND d.id IN (SELECT p0.doc FROM `+rawUnitFrom(units[0], ids, false)+`)
ORDER BY d.created_at DESC, d.id DESC LIMIT 1`, session).Scan(&kind, &src)
	if err != nil {
		return ""
	}
	var label, text string
	switch kind {
	case "prompt":
		label = "[prompt]"
		err = db.QueryRow(`SELECT text FROM prompts WHERE id = ?`, src).Scan(&text)
	case "reply":
		label = "[reply]"
		err = db.QueryRow(`SELECT text FROM replies WHERE id = ?`, src).Scan(&text)
	default: // tool, path
		var name string
		err = db.QueryRow(`SELECT tool_name, input FROM tool_uses WHERE id = ?`, src).Scan(&name, &text)
		label = "[tool " + name + "]"
		if kind == "path" {
			label = "[path " + name + "]"
			text = rawToolPath(name, text)
		}
	}
	if err != nil {
		return ""
	}
	return label + " " + rawExcerpt(text, units[0][0], rawSnippetBefore, rawSnippetAfter)
}

// rawExcerpt collapses whitespace and returns about before+after characters around the first
// whole-word occurrence of term (else the first occurrence, else the start), marking cuts
// with an ellipsis.
func rawExcerpt(text, term string, before, after int) string {
	rs := []rune(strings.Join(strings.Fields(text), " "))
	at := 0
	if term != "" {
		low := make([]rune, len(rs))
		for i, r := range rs {
			low[i] = unicode.ToLower(r)
		}
		if i := rawFindWord(low, []rune(term)); i >= 0 {
			at = i
		}
	}
	start := max(at-before, 0)
	end := min(at+after, len(rs))
	if term == "" {
		start, end = 0, min(after, len(rs))
	}
	out := string(rs[start:end])
	if start > 0 {
		out = "…" + out
	}
	if end < len(rs) {
		out += "…"
	}
	return out
}

// rawFindWord is the index of the first occurrence of term in text standing alone as a word,
// else the first occurrence at all, else -1.
func rawFindWord(text, term []rune) int {
	first := -1
	for i := 0; i+len(term) <= len(text); i++ {
		match := true
		for j, r := range term {
			if text[i+j] != r {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		if first < 0 {
			first = i
		}
		beforeOK := i == 0 || !isRawWordRune(text[i-1])
		afterOK := i+len(term) == len(text) || !isRawWordRune(text[i+len(term)])
		if beforeOK && afterOK {
			return i
		}
	}
	return first
}
