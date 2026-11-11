package main

// Read-only endpoints for the viewer page: facets, a per-day timeseries, system status and the
// stored handoffs. Same rules as memdapi.go: GET only, no raw tool_uses content, every value
// is data for the page to escape.

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

var processStart = time.Now()

// uiTZOffset parses ?tz_offset= (minutes east of UTC, default 0, within +-24 h).
func uiTZOffset(q url.Values) (time.Duration, error) {
	s := q.Get("tz_offset")
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < -1440 || n > 1440 {
		return 0, uiBadRequest("tz_offset must be minutes east of UTC, between -1440 and 1440")
	}
	return time.Duration(n) * time.Minute, nil
}

// uiDateRange turns ?from= and ?to= (YYYY-MM-DD, inclusive civil days in the ?tz_offset= zone)
// into a SQL condition on col. The condition is empty when neither is given.
func uiDateRange(q url.Values, col string) (string, []any, error) {
	off, err := uiTZOffset(q)
	if err != nil {
		return "", nil, err
	}
	var conds []string
	var args []any
	if s := q.Get("from"); s != "" {
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			return "", nil, uiBadRequest("from must be YYYY-MM-DD")
		}
		conds = append(conds, col+` >= CAST(? AS TIMESTAMP)`)
		args = append(args, memdTS(d.Add(-off)))
	}
	if s := q.Get("to"); s != "" {
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			return "", nil, uiBadRequest("to must be YYYY-MM-DD")
		}
		conds = append(conds, col+` < CAST(? AS TIMESTAMP)`)
		args = append(args, memdTS(d.AddDate(0, 0, 1).Add(-off)))
	}
	return strings.Join(conds, " AND "), args, nil
}

// ---- facets ----

type uiFacet struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

func (a *uiAPI) facetList(ctx context.Context, col string) ([]uiFacet, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT `+col+`, CAST(count(*) AS BIGINT) FROM observations
		WHERE `+col+` <> '' GROUP BY 1 ORDER BY 2 DESC, 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []uiFacet{}
	for rows.Next() {
		var f uiFacet
		if err := rows.Scan(&f.Name, &f.Count); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (a *uiAPI) facets(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	projects, err := a.facetList(ctx, "project")
	if err != nil {
		return err
	}
	types, err := a.facetList(ctx, "type")
	if err != nil {
		return err
	}
	var first, last sql.NullTime
	if err := a.db.QueryRowContext(ctx, `SELECT min(created_at), max(created_at) FROM observations`).Scan(&first, &last); err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{
		"projects": projects, "types": types,
		"first_at": uiNullTime(first), "last_at": uiNullTime(last),
	})
	return nil
}

func uiNullTime(t sql.NullTime) string {
	if !t.Valid {
		return ""
	}
	return uiTime(t.Time)
}

// ---- timeseries ----

// timeseries counts observations per day, project and type. Days are civil days in the
// ?tz_offset= zone; from/to bound them as on /observations.
func (a *uiAPI) timeseries(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	off, err := uiTZOffset(q)
	if err != nil {
		return err
	}
	cond, args, err := uiDateRange(q, "created_at")
	if err != nil {
		return err
	}
	var where []string
	if cond != "" {
		where = append(where, cond)
	}
	if p := q.Get("project"); p != "" {
		pm, pa := projectMatch("project", projectKeys(a.db, p))
		where = append(where, pm)
		args = append(args, pa...)
	}
	if ts := q.Get("type"); ts != "" {
		parts := strings.Split(ts, ",")
		where = append(where, `type IN (`+placeholders(len(parts))+`)`)
		for _, p := range parts {
			args = append(args, strings.TrimSpace(p))
		}
	}
	filter := ""
	if len(where) > 0 {
		filter = ` WHERE ` + strings.Join(where, ` AND `)
	}
	args = append([]any{int64(off / time.Minute)}, args...)
	rows, err := a.db.QueryContext(r.Context(), `SELECT day, project, type, CAST(count(*) AS BIGINT) FROM (
		SELECT strftime(created_at + to_minutes(CAST(? AS BIGINT)), '%Y-%m-%d') AS day, project, type
		FROM observations`+filter+`) GROUP BY 1, 2, 3 ORDER BY 1, 2, 3`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	type item struct {
		Day     string `json:"day"`
		Project string `json:"project"`
		Type    string `json:"type"`
		Count   int64  `json:"count"`
	}
	items := []item{}
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.Day, &it.Project, &it.Type, &it.Count); err != nil {
			return err
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"items": items})
	return nil
}

// ---- system ----

// buildInfo is the release the binary was built from ("dev" for an untagged build) and its
// short commit, from the Go build info. The memory binary carries no release stamp of its own.
func buildInfo() (version, rev string) {
	version = "dev"
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	if v := strings.TrimPrefix(bi.Main.Version, "v"); v != "" && !strings.HasPrefix(v, "(devel)") && !strings.Contains(v, "-0.") {
		version = v
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 7 {
			rev = s.Value[:7]
		}
	}
	return
}

func (a *uiAPI) system(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	var obs, sums, prompts, tools, sessions int64
	var lastObs, lastTool, lastPrompt, lastSum, lastSeen sql.NullTime
	if err := a.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM observations), (SELECT count(*) FROM summaries), (SELECT count(*) FROM prompts),
		(SELECT count(*) FROM tool_uses), (SELECT count(*) FROM sessions WHERE NOT is_observer),
		(SELECT max(created_at) FROM observations), (SELECT max(created_at) FROM tool_uses),
		(SELECT max(created_at) FROM prompts), (SELECT max(created_at) FROM summaries),
		(SELECT max(last_seen_at) FROM sessions WHERE NOT is_observer)`).
		Scan(&obs, &sums, &prompts, &tools, &sessions, &lastObs, &lastTool, &lastPrompt, &lastSum, &lastSeen); err != nil {
		return err
	}
	// Ingest is what the transcript tailer writes: tool uses and prompts.
	var ingest sql.NullTime
	for _, t := range []sql.NullTime{lastTool, lastPrompt} {
		if t.Valid && (!ingest.Valid || t.Time.After(ingest.Time)) {
			ingest = t
		}
	}
	var dbBytes, walBytes int64
	path := filepath.Join(memdDir(), "memory.duckdb")
	if fi, err := os.Stat(path); err == nil {
		dbBytes = fi.Size()
	}
	if fi, err := os.Stat(path + ".wal"); err == nil {
		walBytes = fi.Size()
	}
	health := a.healthState()
	version, rev := buildInfo()
	writeJSON(w, 200, map[string]any{
		"version": version, "revision": rev, "started_at": uiTime(processStart),
		"status": health.Status, "problems": health.Problems,
		"db_bytes": dbBytes, "wal_bytes": walBytes,
		"last_ingest_at": uiNullTime(ingest), "last_observation_at": uiNullTime(lastObs),
		"last_summary_at": uiNullTime(lastSum), "last_session_seen_at": uiNullTime(lastSeen),
		"totals": map[string]int64{"observations": obs, "summaries": sums, "prompts": prompts, "raw_tool_uses": tools, "sessions": sessions},
	})
	return nil
}

// ---- handoffs ----

var handoffFileSession = regexp.MustCompile(`HANDOFF-([0-9a-fA-F]{8})`)

type uiHandoff struct {
	ID          string `json:"id"`
	Project     string `json:"project"`
	CreatedAt   string `json:"created_at"`
	Text        string `json:"text"`
	File        string `json:"file"`
	SessionID   string `json:"session_id"`
	SessionLink string `json:"session_link"` // "field", "filename" or ""
	State       string `json:"state"`        // "shown" once a later consumed_at row exists, else "unshown"
	ConsumedAt  string `json:"consumed_at"`
	ConsumedBy  string `json:"consumed_by"`
}

// handoffs lists the stored handoff entries (work_state rows of list "handoff" that carry text),
// newest first. A later consumed_at row marks the entries before it as shown. The session link
// is the row's own session_id when present, else the session whose id starts with the 8 hex
// characters in a HANDOFF-<id>.md file name.
func (a *uiAPI) handoffs(w http.ResponseWriter, r *http.Request) error {
	limit, err := uiLimit(r)
	if err != nil {
		return err
	}
	where, args := `lower(list_name) = ?`, []any{wsHandoffList}
	order := `project, id`
	filtered := false
	if p := r.URL.Query().Get("project"); p != "" {
		// A project and its aliased old keys read as one list, in id order, so a consumed row
		// written under the new key closes a handoff written under the old one.
		pc, pa := projAnd(a.db, "project", p)
		where += pc
		args = append(args, pa...)
		order, filtered = `id`, true
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT id, project, fields, created_at FROM work_state
		WHERE `+where+` ORDER BY `+order, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	var items []*uiHandoff
	var open []*uiHandoff // entries of the current project not yet followed by a consumed row
	cur := ""
	for rows.Next() {
		var id int64
		var project, raw string
		var at time.Time
		if err := rows.Scan(&id, &project, &raw, &at); err != nil {
			return err
		}
		if !filtered && project != cur {
			cur, open = project, nil
		}
		var f map[string]any
		if json.Unmarshal([]byte(raw), &f) != nil {
			continue
		}
		if text, ok := f["text"].(string); ok && strings.TrimSpace(text) != "" {
			h := &uiHandoff{ID: strconv.FormatInt(id, 10), Project: project, CreatedAt: uiTime(at), Text: text, State: "unshown"}
			h.File, _ = f["handoff_file"].(string)
			if sid, _ := f["session_id"].(string); sid != "" {
				h.SessionID, h.SessionLink = sid, "field"
			}
			items = append(items, h)
			open = append(open, h)
			continue
		}
		if ms, ok := f["consumed_at"].(float64); ok {
			by, _ := f["consumed_by"].(string)
			for _, h := range open {
				h.State, h.ConsumedBy, h.ConsumedAt = "shown", by, uiTime(time.UnixMilli(int64(ms)))
			}
			open = nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for _, h := range items {
		if h.SessionID != "" {
			continue
		}
		if m := handoffFileSession.FindStringSubmatch(h.File); m != nil {
			var sid string
			err := a.db.QueryRowContext(r.Context(), `SELECT session_id FROM sessions WHERE NOT is_observer
				AND lower(session_id) LIKE ? ORDER BY started_at DESC LIMIT 1`, strings.ToLower(m[1])+"%").Scan(&sid)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			if sid != "" {
				h.SessionID, h.SessionLink = sid, "filename"
			}
		}
	}
	sortHandoffs(items)
	if len(items) > limit {
		items = items[:limit]
	}
	if items == nil {
		items = []*uiHandoff{}
	}
	writeJSON(w, 200, map[string]any{"items": items})
	return nil
}

// sortHandoffs orders newest first (created_at is RFC 3339 UTC, so string order is time order).
func sortHandoffs(items []*uiHandoff) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && (items[j].CreatedAt > items[j-1].CreatedAt ||
			items[j].CreatedAt == items[j-1].CreatedAt && items[j].ID > items[j-1].ID); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}
