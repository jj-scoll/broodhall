package main

// Read-only UI API served on a TCP port bound to 127.0.0.1 (the daemon task mounts it).
// Security properties, each pinned by memdapi_test.go:
//   - only GET is accepted; every other method is 405
//   - the Host header must be 127.0.0.1[:port] or localhost[:port]; anything else is 403,
//     checked before routing (DNS rebinding defense)
//   - raw tool_uses content (input, output) is never selected by any query here; only counts
//     and tool_name are
//   - responses carry a CSP that allows nothing but same-origin assets
//   - SSE handlers stop on client disconnect and hold no DB connection between ticks

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Opus-equivalent price basis for the cost endpoint, USD per million tokens. Source: the
// Claude API model table (claude-api skill, cached 2026-10-06) for Claude Opus 5.5: input
// $4.00, output $20.00, cache reads $0.20. Cache writes are not listed there; 1.25x input
// ($5.00) is the 5-minute-TTL multiplier and is an assumption. These are list prices, so the
// cost shown is an estimate of what the same tokens would cost on Opus, not what was billed.
const (
	uiPriceInput       = 4.00
	uiPriceCachedInput = 0.20
	uiPriceCacheWrite  = 5.00
	uiPriceOutput      = 20.00
)

const (
	uiDefaultLimit = 50
	uiMaxLimit     = 200
	uiFixText      = "Run `claude` in a terminal, then type `/login`."
)

// uiTick is the SSE poll interval; uiKeepalive the comment interval. Vars so tests can shrink them.
var (
	uiTick      = 2 * time.Second
	uiKeepalive = 15 * time.Second
)

const uiCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// uiHandler serves the read-only UI API under /api/v1/ (and the embedded page at /).
func uiHandler(db *sql.DB) http.Handler {
	a := &uiAPI{db: db}
	mux := http.NewServeMux()
	for path, fn := range map[string]func(http.ResponseWriter, *http.Request) error{
		"/api/v1/health":            a.health,
		"/api/v1/stats":             a.stats,
		"/api/v1/cost":              a.cost,
		"/api/v1/observations":      a.observations,
		"/api/v1/observations/{id}": a.observation,
		"/api/v1/projects":          a.projects,
		"/api/v1/sessions":          a.sessions,
		"/api/v1/sessions/{id}":     a.session,
		"/api/v1/work-state":        a.workState,
		"/api/v1/deliverables":      a.deliverables,
		"/api/v1/observer":          a.observer,
		"/api/v1/privacy":           a.privacy,
		"/api/v1/facets":            a.facets,
		"/api/v1/timeseries":        a.timeseries,
		"/api/v1/system":            a.system,
		"/api/v1/handoffs":          a.handoffs,
	} {
		mux.HandleFunc("GET "+path, wrapAPI(fn))
	}
	mux.HandleFunc("GET /api/v1/events", a.events)
	mux.HandleFunc("GET /api/", func(w http.ResponseWriter, r *http.Request) { uiError(w, 404, "not found") })
	mountUIAssets(mux)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !uiHostOK(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", uiCSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet {
			h.Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// uiHostOK accepts only 127.0.0.1 and localhost, with an optional numeric port.
func uiHostOK(hostport string) bool {
	host := hostport
	if strings.Contains(hostport, ":") {
		h, port, err := net.SplitHostPort(hostport)
		if err != nil || port == "" {
			return false
		}
		for _, c := range port {
			if c < '0' || c > '9' {
				return false
			}
		}
		host = h
	}
	host = strings.ToLower(host)
	return host == "127.0.0.1" || host == "localhost"
}

type uiAPI struct{ db *sql.DB }

type uiBadRequest string

func (e uiBadRequest) Error() string { return string(e) }

type uiNotFound struct{}

func (uiNotFound) Error() string { return "not found" }

func uiError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		code, b = 500, []byte(`{"error":"internal error"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(b)
}

// wrapAPI maps handler errors to statuses; internals are logged, never sent to the page.
func wrapAPI(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		err := fn(w, r)
		switch e := err.(type) {
		case nil:
		case uiBadRequest:
			uiError(w, 400, string(e))
		case uiNotFound:
			uiError(w, 404, "not found")
		default:
			log.Printf("memory ui api %s: %v", r.URL.Path, err)
			uiError(w, 500, "internal error")
		}
	}
}

func uiTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// uiLimit parses ?limit= (default 50, max 200).
func uiLimit(r *http.Request) (int, error) {
	s := r.URL.Query().Get("limit")
	if s == "" {
		return uiDefaultLimit, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, uiBadRequest("limit must be a positive integer")
	}
	return min(n, uiMaxLimit), nil
}

// uiSince turns today|7d|30d into a UTC timestamp string (today = local midnight).
func uiSince(v string, now time.Time) (string, error) {
	switch v {
	case "", "today":
		y, m, d := now.Date()
		return memdTS(time.Date(y, m, d, 0, 0, 0, 0, now.Location())), nil
	case "7d":
		return memdTS(now.AddDate(0, 0, -7)), nil
	case "30d":
		return memdTS(now.AddDate(0, 0, -30)), nil
	}
	return "", uiBadRequest("range must be today, 7d or 30d")
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// ---- health ----

type uiProblem struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Text     string `json:"text"`
	Fix      string `json:"fix"`
	Since    string `json:"since,omitempty"`
}

type uiHealth struct {
	Status         string      `json:"status"`
	ObserverSigned bool        `json:"observer_signed_out"`
	Waiting        int         `json:"waiting"`
	Problems       []uiProblem `json:"problems"`
}

func (a *uiAPI) healthState() uiHealth {
	signedOut, since, waiting := observerState(a.db)
	h := uiHealth{Status: "ok", ObserverSigned: signedOut, Waiting: waiting, Problems: []uiProblem{}}
	if signedOut {
		h.Status = "degraded"
		text := fmt.Sprintf("Observer signed out; %d waiting", waiting)
		if !since.IsZero() {
			text = fmt.Sprintf("Observer signed out since %s; %d waiting", since.Local().Format("15:04"), waiting)
		}
		h.Problems = append(h.Problems, uiProblem{ID: "observer_auth", Severity: "error", Text: text, Fix: uiFixText, Since: uiTime(since)})
	}
	return h
}

func (a *uiAPI) health(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, 200, a.healthState())
	return nil
}

// ---- stats ----

func (a *uiAPI) dupKeys(ctx context.Context) (n int, err error) {
	err = a.db.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT dedupe_key FROM queue
		WHERE dedupe_key <> '' AND input_tokens + output_tokens > 0 GROUP BY dedupe_key HAVING count(*) > 1)`).Scan(&n)
	return n, err
}

func (a *uiAPI) stats(w http.ResponseWriter, r *http.Request) error {
	since, err := uiSince(r.URL.Query().Get("since"), time.Now())
	if err != nil {
		return err
	}
	ctx := r.Context()
	var memories, toolUses, calls int64
	if err := a.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM observations WHERE created_at >= CAST(? AS TIMESTAMP)),
		(SELECT count(*) FROM tool_uses WHERE created_at >= CAST(? AS TIMESTAMP)),
		(SELECT CAST(coalesce(sum(attempts), 0) AS BIGINT) FROM queue WHERE updated_at >= CAST(? AS TIMESTAMP))`,
		since, since, since).Scan(&memories, &toolUses, &calls); err != nil {
		return err
	}
	var totals [4]int64
	if err := a.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM observations),
		(SELECT count(*) FROM summaries), (SELECT count(*) FROM prompts), (SELECT count(*) FROM tool_uses)`).
		Scan(&totals[0], &totals[1], &totals[2], &totals[3]); err != nil {
		return err
	}
	dups, err := a.dupKeys(ctx)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{
		"memories": memories, "tool_uses_seen": toolUses, "model_calls": calls, "duplicate_call_keys": dups,
		"totals": map[string]int64{"observations": totals[0], "summaries": totals[1], "prompts": totals[2], "raw_tool_uses": totals[3]},
	})
	return nil
}

// ---- cost ----

type uiTokens struct {
	Input      int64 `json:"input"`
	CachedRead int64 `json:"cached_input"`
	CacheWrite int64 `json:"cache_write"`
	Output     int64 `json:"output"`
}

func (t uiTokens) usdByKind() map[string]float64 {
	return map[string]float64{
		"input":        float64(t.Input) / 1e6 * uiPriceInput,
		"cached_input": float64(t.CachedRead) / 1e6 * uiPriceCachedInput,
		"cache_write":  float64(t.CacheWrite) / 1e6 * uiPriceCacheWrite,
		"output":       float64(t.Output) / 1e6 * uiPriceOutput,
	}
}

func (t uiTokens) usd() float64 {
	var s float64
	for _, v := range t.usdByKind() {
		s += v
	}
	return s
}

type uiCostRow struct {
	Key      string  `json:"key"`
	Memories int     `json:"memories"`
	Calls    int     `json:"calls"`
	USD      float64 `json:"usd"`
	tokens   uiTokens
}

// costRows groups queue token usage since a timestamp by project, model or day.
func (a *uiAPI) costRows(ctx context.Context, since, group string) ([]uiCostRow, error) {
	var key, join string
	switch group {
	case "", "project":
		key = `coalesce(nullif(s.project, ''), '(unknown)')`
		join = `LEFT JOIN sessions s ON s.session_id = q.session_id`
	case "model":
		key = `coalesce(nullif(o.m, ''), '(unknown)')`
		join = `LEFT JOIN (SELECT dedupe_key, min(generated_by_model) AS m FROM observations
			WHERE dedupe_key <> '' GROUP BY dedupe_key) o ON o.dedupe_key = q.dedupe_key AND q.dedupe_key <> ''`
	case "day":
		key = `strftime(q.updated_at, '%Y-%m-%d')`
	default:
		return nil, uiBadRequest("group must be project, model or day")
	}
	rows, err := a.db.QueryContext(ctx, `SELECT `+key+` AS k,
		CAST(count(*) FILTER (WHERE q.state = 'done') AS INTEGER),
		CAST(count(*) FILTER (WHERE q.input_tokens + q.output_tokens + q.cache_read_tokens + q.cache_creation_tokens > 0) AS INTEGER),
		CAST(coalesce(sum(q.input_tokens), 0) AS BIGINT), CAST(coalesce(sum(q.cache_read_tokens), 0) AS BIGINT),
		CAST(coalesce(sum(q.cache_creation_tokens), 0) AS BIGINT), CAST(coalesce(sum(q.output_tokens), 0) AS BIGINT)
		FROM queue q `+join+` WHERE q.updated_at >= CAST(? AS TIMESTAMP) GROUP BY k`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []uiCostRow{}
	for rows.Next() {
		var c uiCostRow
		if err := rows.Scan(&c.Key, &c.Memories, &c.Calls, &c.tokens.Input, &c.tokens.CachedRead, &c.tokens.CacheWrite, &c.tokens.Output); err != nil {
			return nil, err
		}
		c.USD = c.tokens.usd()
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if group == "day" {
			return out[i].Key > out[j].Key
		}
		if out[i].USD != out[j].USD {
			return out[i].USD > out[j].USD
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}

func (a *uiAPI) cost(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	since, err := uiSince(q.Get("range"), time.Now())
	if err != nil {
		return err
	}
	rows, err := a.costRows(r.Context(), since, q.Get("group"))
	if err != nil {
		return err
	}
	var tot uiTokens
	for _, c := range rows {
		tot.Input += c.tokens.Input
		tot.CachedRead += c.tokens.CachedRead
		tot.CacheWrite += c.tokens.CacheWrite
		tot.Output += c.tokens.Output
	}
	writeJSON(w, 200, map[string]any{
		"price_basis": "opus_equivalent", "estimated": true, "usd": tot.usd(), "tokens": tot,
		"usd_by_kind": tot.usdByKind(), "rows": rows,
		"prices_per_mtok": map[string]float64{"input": uiPriceInput, "cached_input": uiPriceCachedInput,
			"cache_write": uiPriceCacheWrite, "output": uiPriceOutput},
	})
	return nil
}

// ---- observations ----

type uiObservation struct {
	ID        string   `json:"id"`
	Type      string   `json:"type"`
	Project   string   `json:"project"`
	SessionID string   `json:"session_id"`
	Title     string   `json:"title"`
	Subtitle  string   `json:"subtitle"`
	CreatedAt string   `json:"created_at"`
	Facts     []string `json:"facts"`
	Concepts  []string `json:"concepts"`
	FilesRead []string `json:"files_read"`
	FilesMod  []string `json:"files_modified"`
	Model     string   `json:"generated_by_model"`
	PromptVer string   `json:"prompt_version"`
	RawType   string   `json:"raw_type"`
	Narrative string   `json:"narrative,omitempty"`
	numericID int64
}

const uiObsCols = `id, project, session_id, type, raw_type, title, subtitle, narrative, facts, concepts,
	prompt_version, generated_by_model, created_at`

func jsonStrings(s string) []string {
	out := []string{}
	_ = json.Unmarshal([]byte(s), &out)
	if out == nil {
		out = []string{}
	}
	return out
}

func scanObservations(rows *sql.Rows, withNarrative bool) ([]*uiObservation, error) {
	var out []*uiObservation
	for rows.Next() {
		var o uiObservation
		var narr, facts, concepts string
		var at time.Time
		if err := rows.Scan(&o.numericID, &o.Project, &o.SessionID, &o.Type, &o.RawType, &o.Title, &o.Subtitle,
			&narr, &facts, &concepts, &o.PromptVer, &o.Model, &at); err != nil {
			return nil, err
		}
		o.ID = strconv.FormatInt(o.numericID, 10)
		o.CreatedAt = uiTime(at)
		o.Facts, o.Concepts = jsonStrings(facts), jsonStrings(concepts)
		o.FilesRead, o.FilesMod = []string{}, []string{}
		if withNarrative {
			o.Narrative = narr
		}
		out = append(out, &o)
	}
	return out, rows.Err()
}

func (a *uiAPI) attachFiles(ctx context.Context, items []*uiObservation) error {
	if len(items) == 0 {
		return nil
	}
	byID := map[int64]*uiObservation{}
	args := make([]any, 0, len(items))
	for _, o := range items {
		byID[o.numericID] = o
		args = append(args, o.numericID)
	}
	rows, err := a.db.QueryContext(ctx, `SELECT observation_id, path, kind FROM observation_files
		WHERE observation_id IN (`+placeholders(len(args))+`) ORDER BY path`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var path, kind string
		if err := rows.Scan(&id, &path, &kind); err != nil {
			return err
		}
		if o := byID[id]; o != nil {
			if kind == "read" {
				o.FilesRead = append(o.FilesRead, path)
			} else {
				o.FilesMod = append(o.FilesMod, path)
			}
		}
	}
	return rows.Err()
}

func (a *uiAPI) observations(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	limit, err := uiLimit(r)
	if err != nil {
		return err
	}
	var where []string
	var args []any
	if s := strings.TrimSpace(q.Get("q")); s != "" {
		where = append(where, `lower(title || ' ' || subtitle || ' ' || narrative || ' ' || facts || ' ' || concepts) LIKE ? ESCAPE '\'`)
		args = append(args, "%"+likeEscape(strings.ToLower(s))+"%")
	}
	if p := q.Get("project"); p != "" {
		where = append(where, `project = ?`)
		args = append(args, p)
	}
	if ts := q.Get("type"); ts != "" {
		parts := strings.Split(ts, ",")
		where = append(where, `type IN (`+placeholders(len(parts))+`)`)
		for _, p := range parts {
			args = append(args, strings.TrimSpace(p))
		}
	}
	if s := q.Get("session"); s != "" {
		where = append(where, `session_id = ?`)
		args = append(args, s)
	}
	dcond, dargs, err := uiDateRange(q, "created_at")
	if err != nil {
		return err
	}
	if dcond != "" {
		where = append(where, dcond)
		args = append(args, dargs...)
	}
	filter := ""
	if len(where) > 0 {
		filter = ` WHERE ` + strings.Join(where, ` AND `)
	}
	ctx := r.Context()
	var total int
	if err := a.db.QueryRowContext(ctx, `SELECT count(*) FROM observations`+filter, args...).Scan(&total); err != nil {
		return err
	}
	pageWhere, pageArgs := filter, append([]any(nil), args...)
	if c := q.Get("cursor"); c != "" {
		id, err := strconv.ParseInt(c, 10, 64)
		if err != nil || id < 1 {
			return uiBadRequest("invalid cursor")
		}
		if pageWhere == "" {
			pageWhere = ` WHERE id < ?`
		} else {
			pageWhere += ` AND id < ?`
		}
		pageArgs = append(pageArgs, id)
	}
	pageArgs = append(pageArgs, limit+1)
	rows, err := a.db.QueryContext(ctx, `SELECT `+uiObsCols+` FROM observations`+pageWhere+` ORDER BY id DESC LIMIT ?`, pageArgs...)
	if err != nil {
		return err
	}
	items, err := scanObservations(rows, false)
	rows.Close()
	if err != nil {
		return err
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = items[len(items)-1].ID
	}
	if err := a.attachFiles(ctx, items); err != nil {
		return err
	}
	if items == nil {
		items = []*uiObservation{}
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_cursor": next, "total": total})
	return nil
}

func (a *uiAPI) observation(w http.ResponseWriter, r *http.Request) error {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return uiNotFound{}
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT `+uiObsCols+` FROM observations WHERE id = ?`, id)
	if err != nil {
		return err
	}
	items, err := scanObservations(rows, true)
	rows.Close()
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return uiNotFound{}
	}
	if err := a.attachFiles(r.Context(), items); err != nil {
		return err
	}
	writeJSON(w, 200, items[0])
	return nil
}

func (a *uiAPI) projects(w http.ResponseWriter, r *http.Request) error {
	rows, err := a.db.QueryContext(r.Context(), `SELECT project, CAST(sum(n) AS BIGINT) FROM (
		SELECT project, count(*) AS n FROM observations GROUP BY project
		UNION ALL SELECT project, 0 FROM work_state GROUP BY project) WHERE project <> '' AND project <> ?
		GROUP BY project ORDER BY 2 DESC, 1`, wsUserProject)
	if err != nil {
		return err
	}
	defer rows.Close()
	type pj struct {
		Name  string `json:"name"`
		Count int64  `json:"count"`
	}
	items := []pj{}
	for rows.Next() {
		var p pj
		if err := rows.Scan(&p.Name, &p.Count); err != nil {
			return err
		}
		items = append(items, p)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"items": items})
	return nil
}

// ---- sessions ----

type uiSummary struct {
	Request      string `json:"request"`
	Investigated string `json:"investigated"`
	Learned      string `json:"learned"`
	Completed    string `json:"completed"`
	NextSteps    string `json:"next_steps"`
	Notes        string `json:"notes"`
}

type uiSession struct {
	ID          string    `json:"id"`
	Project     string    `json:"project"`
	StartedAt   string    `json:"started_at"`
	EndedAt     string    `json:"ended_at"`
	ToolUses    int       `json:"tool_uses"`
	USD         float64   `json:"usd"`
	PromptCount int       `json:"prompt_count"`
	Summary     uiSummary `json:"summary"`
}

const uiSessionSelect = `SELECT s.session_id, s.project, s.started_at, s.last_seen_at,
	CAST((SELECT count(*) FROM tool_uses t WHERE t.session_id = s.session_id) AS INTEGER),
	CAST((SELECT count(*) FROM prompts p WHERE p.session_id = s.session_id) AS INTEGER),
	CAST((SELECT coalesce(sum(input_tokens), 0) FROM queue q WHERE q.session_id = s.session_id) AS BIGINT),
	CAST((SELECT coalesce(sum(cache_read_tokens), 0) FROM queue q WHERE q.session_id = s.session_id) AS BIGINT),
	CAST((SELECT coalesce(sum(cache_creation_tokens), 0) FROM queue q WHERE q.session_id = s.session_id) AS BIGINT),
	CAST((SELECT coalesce(sum(output_tokens), 0) FROM queue q WHERE q.session_id = s.session_id) AS BIGINT)
	FROM sessions s`

func scanSessions(rows *sql.Rows) ([]*uiSession, error) {
	var out []*uiSession
	for rows.Next() {
		var s uiSession
		var started, ended time.Time
		var tk uiTokens
		if err := rows.Scan(&s.ID, &s.Project, &started, &ended, &s.ToolUses, &s.PromptCount,
			&tk.Input, &tk.CachedRead, &tk.CacheWrite, &tk.Output); err != nil {
			return nil, err
		}
		s.StartedAt, s.EndedAt, s.USD = uiTime(started), uiTime(ended), tk.usd()
		out = append(out, &s)
	}
	return out, rows.Err()
}

func (a *uiAPI) attachSummaries(ctx context.Context, items []*uiSession) error {
	if len(items) == 0 {
		return nil
	}
	by := map[string]*uiSession{}
	args := make([]any, 0, len(items))
	for _, s := range items {
		by[s.ID] = s
		args = append(args, s.ID)
	}
	rows, err := a.db.QueryContext(ctx, `SELECT session_id, request, investigated, learned, completed, next_steps, notes
		FROM summaries WHERE session_id IN (`+placeholders(len(args))+`) ORDER BY id`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() { // later summaries of a session win
		var id string
		var m uiSummary
		if err := rows.Scan(&id, &m.Request, &m.Investigated, &m.Learned, &m.Completed, &m.NextSteps, &m.Notes); err != nil {
			return err
		}
		if s := by[id]; s != nil {
			s.Summary = m
		}
	}
	return rows.Err()
}

func (a *uiAPI) sessions(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	limit, err := uiLimit(r)
	if err != nil {
		return err
	}
	offset := 0
	if c := q.Get("cursor"); c != "" {
		offset, err = strconv.Atoi(c)
		if err != nil || offset < 0 {
			return uiBadRequest("invalid cursor")
		}
	}
	where, args := ` WHERE NOT s.is_observer`, []any{}
	if p := q.Get("project"); p != "" {
		where += ` AND s.project = ?`
		args = append(args, p)
	}
	args = append(args, limit+1, offset)
	rows, err := a.db.QueryContext(r.Context(), uiSessionSelect+where+` ORDER BY s.started_at DESC, s.session_id LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return err
	}
	items, err := scanSessions(rows)
	rows.Close()
	if err != nil {
		return err
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = strconv.Itoa(offset + limit)
	}
	if err := a.attachSummaries(r.Context(), items); err != nil {
		return err
	}
	if items == nil {
		items = []*uiSession{}
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_cursor": next})
	return nil
}

func (a *uiAPI) session(w http.ResponseWriter, r *http.Request) error {
	ctx, id := r.Context(), r.PathValue("id")
	rows, err := a.db.QueryContext(ctx, uiSessionSelect+` WHERE s.session_id = ?`, id)
	if err != nil {
		return err
	}
	items, err := scanSessions(rows)
	rows.Close()
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return uiNotFound{}
	}
	if err := a.attachSummaries(ctx, items); err != nil {
		return err
	}
	type prompt struct {
		ID   string `json:"id"`
		Text string `json:"text"`
		At   string `json:"at"`
	}
	prompts, obsIDs := []prompt{}, []string{}
	pr, err := a.db.QueryContext(ctx, `SELECT id, text, created_at FROM prompts WHERE session_id = ? ORDER BY prompt_number, created_at`, id)
	if err != nil {
		return err
	}
	for pr.Next() {
		var p prompt
		var pid int64
		var at time.Time
		if err := pr.Scan(&pid, &p.Text, &at); err != nil {
			pr.Close()
			return err
		}
		p.ID, p.At = strconv.FormatInt(pid, 10), uiTime(at)
		prompts = append(prompts, p)
	}
	pr.Close()
	if err := pr.Err(); err != nil {
		return err
	}
	or, err := a.db.QueryContext(ctx, `SELECT id FROM observations WHERE session_id = ? ORDER BY id`, id)
	if err != nil {
		return err
	}
	defer or.Close()
	for or.Next() {
		var oid int64
		if err := or.Scan(&oid); err != nil {
			return err
		}
		obsIDs = append(obsIDs, strconv.FormatInt(oid, 10))
	}
	if err := or.Err(); err != nil {
		return err
	}
	writeJSON(w, 200, struct {
		*uiSession
		Prompts        []prompt `json:"prompts"`
		ObservationIDs []string `json:"observation_ids"`
	}{items[0], prompts, obsIDs})
	return nil
}

// ---- work state, deliverables ----

func (a *uiAPI) workState(w http.ResponseWriter, r *http.Request) error {
	projects := []string{r.URL.Query().Get("project")}
	if projects[0] == "" {
		rows, err := a.db.QueryContext(r.Context(), `SELECT DISTINCT project FROM work_state
			WHERE project <> '' AND project <> ? ORDER BY project`, wsUserProject)
		if err != nil {
			return err
		}
		projects = nil
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return err
			}
			projects = append(projects, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	type list struct {
		Name    string           `json:"name"`
		Project string           `json:"project"`
		Items   []map[string]any `json:"items"`
	}
	lists := []list{}
	userDone := false
	for _, p := range projects {
		folded, err := workStateFold(a.db, p)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(folded))
		for n := range folded {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if n == wsUserFactsList {
				if userDone {
					continue
				}
				userDone = true
			}
			lists = append(lists, list{Name: n, Project: p, Items: folded[n]})
		}
	}
	writeJSON(w, 200, map[string]any{"lists": lists})
	return nil
}

func (a *uiAPI) deliverables(w http.ResponseWriter, r *http.Request) error {
	limit, err := uiLimit(r)
	if err != nil {
		return err
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT project, path, created_at FROM deliverables ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	type item struct {
		Title    string `json:"title"`
		Location string `json:"location"`
		Project  string `json:"project"`
		At       string `json:"at"`
	}
	items := []item{}
	for rows.Next() {
		var it item
		var at time.Time
		if err := rows.Scan(&it.Project, &it.Location, &at); err != nil {
			return err
		}
		it.Title, it.At = filepath.Base(it.Location), uiTime(at)
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"items": items})
	return nil
}

// ---- observer, privacy ----

func (a *uiAPI) queueCounts(ctx context.Context) (map[string]int, error) {
	counts := map[string]int{"queued": 0, "claimed": 0, "done": 0, "rejected": 0, "unknown": 0}
	rows, err := a.db.QueryContext(ctx, `SELECT state, CAST(count(*) AS INTEGER) FROM queue GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		counts[s] = n
	}
	return counts, rows.Err()
}

func (a *uiAPI) observer(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	signedOut, since, _ := observerState(a.db)
	counts, err := a.queueCounts(ctx)
	if err != nil {
		return err
	}
	var oldest sql.NullTime
	if err := a.db.QueryRowContext(ctx, `SELECT min(created_at) FROM queue WHERE state = 'queued'`).Scan(&oldest); err != nil {
		return err
	}
	age := int64(0)
	if oldest.Valid {
		age = max(0, int64(time.Since(oldest.Time.UTC()).Seconds()))
	}
	type rej struct {
		Reason string `json:"reason"`
		Count  int    `json:"count"`
	}
	rejections := []rej{}
	rr, err := a.db.QueryContext(ctx, `SELECT reason, CAST(count(*) AS INTEGER) FROM queue WHERE state = 'rejected'
		GROUP BY reason ORDER BY 2 DESC, 1`)
	if err != nil {
		return err
	}
	for rr.Next() {
		var x rej
		if err := rr.Scan(&x.Reason, &x.Count); err != nil {
			rr.Close()
			return err
		}
		rejections = append(rejections, x)
	}
	rr.Close()
	if err := rr.Err(); err != nil {
		return err
	}
	type rejItem struct {
		At     string `json:"at"`
		Tool   string `json:"tool"`
		Reason string `json:"reason"`
	}
	items := []rejItem{}
	ri, err := a.db.QueryContext(ctx, `SELECT q.updated_at, coalesce(t.tool_name, ''), q.reason FROM queue q
		LEFT JOIN tool_uses t ON t.session_id = q.session_id AND t.tool_use_id = q.tool_use_id
		WHERE q.state = 'rejected' ORDER BY q.updated_at DESC, q.id DESC LIMIT 50`)
	if err != nil {
		return err
	}
	for ri.Next() {
		var x rejItem
		var at time.Time
		if err := ri.Scan(&at, &x.Tool, &x.Reason); err != nil {
			ri.Close()
			return err
		}
		x.At = uiTime(at)
		items = append(items, x)
	}
	ri.Close()
	if err := ri.Err(); err != nil {
		return err
	}
	dups, err := a.dupKeys(ctx)
	if err != nil {
		return err
	}
	since30, _ := uiSince("30d", time.Now())
	byModel, err := a.costRows(ctx, since30, "model")
	if err != nil {
		return err
	}
	type modelRow struct {
		Model string  `json:"model"`
		Calls int     `json:"calls"`
		USD   float64 `json:"usd"`
	}
	models := []modelRow{}
	for _, c := range byModel {
		models = append(models, modelRow{c.Key, c.Calls, c.USD})
	}
	auth := "ok"
	if signedOut {
		auth = "invalid"
	}
	writeJSON(w, 200, map[string]any{
		"auth": auth, "auth_since": uiTime(since), "auth_fix": uiFixText,
		"queue":      map[string]any{"queued": counts["queued"], "claimed": counts["claimed"], "done": counts["done"], "rejected": counts["rejected"], "unknown": counts["unknown"], "oldest_queued_age_s": age},
		"rejections": rejections, "rejected_items": items,
		"dedupe": map[string]int{"keys_with_multiple_calls": dups},
		"models": models, "models_range": "30d",
	})
	return nil
}

func (a *uiAPI) privacy(w http.ResponseWriter, r *http.Request) error {
	var size int64
	path := filepath.Join(memdDir(), "memory.duckdb")
	if fi, err := os.Stat(path); err == nil {
		size = fi.Size()
	}
	writeJSON(w, 200, map[string]any{
		"bind": r.Host, "raw_tool_uses_enabled": false, "raw_tool_uses_served": false,
		"db_path": path, "db_bytes": size,
	})
	return nil
}

// ---- SSE ----

// fingerprint is a cheap change detector for the invalidate event.
type uiFingerprint struct{ obs, sums, prompts, work, deliv, done int64 }

func (a *uiAPI) fingerprint(ctx context.Context) (f uiFingerprint, err error) {
	err = a.db.QueryRowContext(ctx, `SELECT
		CAST((SELECT coalesce(max(id), 0) FROM observations) AS BIGINT),
		CAST((SELECT coalesce(max(id), 0) FROM summaries) AS BIGINT),
		CAST((SELECT coalesce(max(id), 0) FROM prompts) AS BIGINT),
		CAST((SELECT coalesce(max(id), 0) FROM work_state) AS BIGINT),
		CAST((SELECT coalesce(max(id), 0) FROM deliverables) AS BIGINT),
		CAST((SELECT count(*) FROM queue WHERE state = 'done') AS BIGINT)`).
		Scan(&f.obs, &f.sums, &f.prompts, &f.work, &f.deliv, &f.done)
	return f, err
}

func (f uiFingerprint) views(prev uiFingerprint) []string {
	var v []string
	if f.obs != prev.obs {
		v = append(v, "memory", "overview")
	}
	if f.sums != prev.sums || f.prompts != prev.prompts {
		v = append(v, "sessions")
	}
	if f.work != prev.work || f.deliv != prev.deliv {
		v = append(v, "work")
	}
	if f.done != prev.done {
		v = append(v, "observer", "overview")
	}
	seen := map[string]bool{}
	out := v[:0]
	for _, x := range v {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func sseSend(w http.ResponseWriter, fl http.Flusher, event string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
		return err
	}
	fl.Flush()
	return nil
}

// events streams small invalidation messages: health (full), counters (queue counts) and
// invalidate ({"views":[...]}). Each tick runs short queries and releases its connection; the
// loop ends when the request context is cancelled (client gone or server shutdown).
func (a *uiAPI) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		uiError(w, 500, "streaming unsupported")
		return
	}
	// The server's write timeout (if any) must not cut a long-lived stream.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(200)

	ctx := r.Context()
	lastHealth, lastCounters := "", ""
	fp, _ := a.fingerprint(ctx)
	emit := func() bool {
		hb, _ := json.Marshal(a.healthState())
		if string(hb) != lastHealth {
			lastHealth = string(hb)
			if sseSend(w, fl, "health", json.RawMessage(hb)) != nil {
				return false
			}
		}
		if counts, err := a.queueCounts(ctx); err == nil {
			cb, _ := json.Marshal(counts)
			if string(cb) != lastCounters {
				lastCounters = string(cb)
				if sseSend(w, fl, "counters", json.RawMessage(cb)) != nil {
					return false
				}
			}
		}
		if next, err := a.fingerprint(ctx); err == nil {
			if views := next.views(fp); len(views) > 0 {
				if sseSend(w, fl, "invalidate", map[string]any{"views": views}) != nil {
					return false
				}
			}
			fp = next
		}
		return true
	}
	if !emit() {
		return
	}
	tick, keep := time.NewTicker(uiTick), time.NewTicker(uiKeepalive)
	defer tick.Stop()
	defer keep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if !emit() {
				return
			}
		case <-keep.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}
