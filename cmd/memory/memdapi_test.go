package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const apiCanary = "CANARY-RAW-TOOL-INPUT-7f3a"

func apiGet(t *testing.T, h http.Handler, target string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	req.Host = "127.0.0.1:37800"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func apiJSON(t *testing.T, h http.Handler, target string, v any) {
	t.Helper()
	code, body := apiGet(t, h, target)
	if code != 200 {
		t.Fatalf("GET %s = %d: %s", target, code, body)
	}
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("GET %s: bad json: %v: %s", target, err, body)
	}
}

func apiExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func seedAPI(t *testing.T, db *sql.DB) {
	t.Helper()
	apiExec(t, db, `INSERT INTO sessions (session_id, project) VALUES ('s1','proj-a'), ('s2','proj-b')`)
	types := []string{"bugfix", "decision", "feature", "bugfix", "discovery"}
	for i, ty := range types {
		apiExec(t, db, `INSERT INTO observations (project, session_id, type, raw_type, title, subtitle, narrative, source)
			VALUES (?, 's1', ?, ?, ?, 'sub', 'narr', 'live')`, "proj-a", ty, ty, "title "+ty+string(rune('A'+i)))
	}
	apiExec(t, db, `INSERT INTO observation_files VALUES (1, 'a.go', 'read'), (1, 'b.go', 'modified')`)
	apiExec(t, db, `INSERT INTO prompts (session_id, prompt_number, text) VALUES ('s1', 1, 'do the thing')`)
	apiExec(t, db, `INSERT INTO summaries (project, session_id, request, source) VALUES ('proj-a','s1','req text','live')`)
	// Raw tool use with a canary in input and output.
	apiExec(t, db, `INSERT INTO tool_uses (session_id, tool_use_id, tool_name, input, output)
		VALUES ('s1','tu1','Bash', ?, ?)`, apiCanary+" input", apiCanary+" output")
	apiExec(t, db, `INSERT INTO queue (session_id, tool_use_id, state, reason, dedupe_key, input_tokens, output_tokens, cache_read_tokens)
		VALUES ('s1','tu1','done','', 'k1', 1000000, 100000, 500000),
		       ('s1','tu2','queued','', '', 0, 0, 0),
		       ('s1','tu1','rejected','schema', 'k2', 0, 0, 0)`)
	apiExec(t, db, `INSERT INTO work_state (project, list_name, fields) VALUES
		('proj-a','todo','{"task":"write it","status":"doing"}'),
		('proj-a','facts','{"k":"v"}')`)
	apiExec(t, db, `INSERT INTO deliverables (project, session_id, path) VALUES ('proj-a','s1','/x/report.md')`)
}

func TestMemdAPIMethodAndHost(t *testing.T) {
	db, _ := openTestMemd(t)
	h := uiHandler(db)
	tests := []struct {
		name, method, host, target string
		want                       int
	}{
		{"get ok", "GET", "127.0.0.1:37800", "/api/v1/health", 200},
		{"get ok no port", "GET", "127.0.0.1", "/api/v1/health", 200},
		{"localhost ok", "GET", "localhost:1", "/api/v1/health", 200},
		{"LOCALHOST ok", "GET", "LocalHost", "/api/v1/health", 200},
		{"post", "POST", "127.0.0.1:37800", "/api/v1/health", 405},
		{"put", "PUT", "127.0.0.1:37800", "/api/v1/observations", 405},
		{"delete", "DELETE", "127.0.0.1:37800", "/api/v1/observations/1", 405},
		{"head", "HEAD", "127.0.0.1:37800", "/api/v1/health", 405},
		{"options", "OPTIONS", "127.0.0.1:37800", "/api/v1/health", 405},
		{"evil host", "GET", "evil.example", "/api/v1/health", 403},
		{"evil host with port", "GET", "evil.example:37800", "/api/v1/health", 403},
		{"suffix trick", "GET", "127.0.0.1.evil.example", "/api/v1/health", 403},
		{"prefix trick", "GET", "localhost.evil.example:80", "/api/v1/health", 403},
		{"ipv6 not allowed", "GET", "[::1]:37800", "/api/v1/health", 403},
		{"empty host", "GET", "", "/api/v1/health", 403},
		{"evil host before method", "POST", "evil.example", "/api/v1/health", 403},
		{"evil host on events", "GET", "evil.example", "/api/v1/events", 403},
		{"unknown path", "GET", "127.0.0.1", "/api/v1/nope", 404},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.target, nil)
			req.Host = tt.host
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Errorf("%s %s Host=%q = %d, want %d", tt.method, tt.target, tt.host, rec.Code, tt.want)
			}
		})
	}
}

func TestMemdAPIHeaders(t *testing.T) {
	db, _ := openTestMemd(t)
	req := httptest.NewRequest("GET", "/api/v1/health", nil)
	req.Host = "127.0.0.1"
	rec := httptest.NewRecorder()
	uiHandler(db).ServeHTTP(rec, req)
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP = %q", csp)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff")
	}
}

func TestMemdAPIObservationsPaging(t *testing.T) {
	db, _ := openTestMemd(t)
	seedAPI(t, db)
	h := uiHandler(db)

	type page struct {
		Items []struct {
			ID    string `json:"id"`
			Type  string `json:"type"`
			Title string `json:"title"`
		} `json:"items"`
		NextCursor string `json:"next_cursor"`
		Total      int    `json:"total"`
	}
	var seen []string
	cursor := ""
	for i := 0; i < 5; i++ {
		var p page
		apiJSON(t, h, "/api/v1/observations?limit=2&cursor="+cursor, &p)
		if p.Total != 5 {
			t.Errorf("total = %d, want 5", p.Total)
		}
		for _, it := range p.Items {
			seen = append(seen, it.ID)
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if got := strings.Join(seen, ","); got != "5,4,3,2,1" {
		t.Errorf("paged ids = %s, want 5,4,3,2,1 (newest first, no repeats)", got)
	}

	var f page
	apiJSON(t, h, "/api/v1/observations?type=bugfix,decision", &f)
	if len(f.Items) != 3 || f.Total != 3 {
		t.Fatalf("type filter: %d items, total %d, want 3/3", len(f.Items), f.Total)
	}
	for _, it := range f.Items {
		if it.Type != "bugfix" && it.Type != "decision" {
			t.Errorf("unexpected type %q", it.Type)
		}
	}

	var q page
	apiJSON(t, h, "/api/v1/observations?q=featurec", &q)
	if len(q.Items) != 1 {
		t.Errorf("q filter: %d items, want 1", len(q.Items))
	}
	if code, _ := apiGet(t, h, "/api/v1/observations?limit=abc"); code != 400 {
		t.Errorf("bad limit = %d, want 400", code)
	}
	if code, _ := apiGet(t, h, "/api/v1/observations?cursor=zz"); code != 400 {
		t.Errorf("bad cursor = %d, want 400", code)
	}

	var one struct {
		ID           string   `json:"id"`
		Narrative    string   `json:"narrative"`
		FilesRead    []string `json:"files_read"`
		FilesModded  []string `json:"files_modified"`
		Facts        []string `json:"facts"`
		GeneratedBy  string   `json:"generated_by_model"`
		PromptVerion string   `json:"prompt_version"`
	}
	apiJSON(t, h, "/api/v1/observations/1", &one)
	if one.Narrative != "narr" || len(one.FilesRead) != 1 || len(one.FilesModded) != 1 || one.Facts == nil {
		t.Errorf("detail = %+v", one)
	}
	if code, _ := apiGet(t, h, "/api/v1/observations/999"); code != 404 {
		t.Errorf("missing observation = %d, want 404", code)
	}
}

func TestMemdAPINoRawToolUses(t *testing.T) {
	db, _ := openTestMemd(t)
	seedAPI(t, db)
	h := uiHandler(db)
	paths := []string{
		"/api/v1/health", "/api/v1/stats", "/api/v1/stats?since=30d",
		"/api/v1/cost", "/api/v1/cost?range=7d&group=project", "/api/v1/cost?group=model", "/api/v1/cost?group=day",
		"/api/v1/observations", "/api/v1/observations/1", "/api/v1/projects",
		"/api/v1/sessions", "/api/v1/sessions/s1", "/api/v1/work-state", "/api/v1/work-state?project=proj-a",
		"/api/v1/deliverables", "/api/v1/observer", "/api/v1/privacy",
		"/api/v1/facets", "/api/v1/timeseries", "/api/v1/system", "/api/v1/handoffs",
	}
	for _, p := range paths {
		code, body := apiGet(t, h, p)
		if code != 200 {
			t.Errorf("GET %s = %d: %s", p, code, body)
		}
		if strings.Contains(body, apiCanary) {
			t.Errorf("GET %s leaks raw tool use content", p)
		}
		if !json.Valid([]byte(body)) {
			t.Errorf("GET %s not valid JSON", p)
		}
	}
	// The SSE stream too.
	srv := httptest.NewServer(h)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	if strings.Contains(string(buf[:n]), apiCanary) {
		t.Error("SSE leaks raw tool use content")
	}
}

func TestMemdAPIHealthSignedOut(t *testing.T) {
	db, _ := openTestMemd(t)
	h := uiHandler(db)

	var ok struct {
		Status    string `json:"status"`
		SignedOut bool   `json:"observer_signed_out"`
		Problems  []any  `json:"problems"`
	}
	apiJSON(t, h, "/api/v1/health", &ok)
	if ok.Status != "ok" || ok.SignedOut || len(ok.Problems) != 0 {
		t.Errorf("healthy = %+v", ok)
	}

	apiExec(t, db, `INSERT INTO queue (session_id, tool_use_id, state) VALUES ('s','a','queued'), ('s','b','unknown'), ('s','c','done')`)
	apiExec(t, db, `INSERT INTO observer_health (id, last_outcome, last_reason) VALUES (1, 'unknown', 'auth')`)
	var bad struct {
		Status    string `json:"status"`
		SignedOut bool   `json:"observer_signed_out"`
		Waiting   int    `json:"waiting"`
		Problems  []struct {
			ID, Severity, Text, Fix string
		} `json:"problems"`
	}
	apiJSON(t, h, "/api/v1/health", &bad)
	if bad.Status != "degraded" || !bad.SignedOut || bad.Waiting != 2 || len(bad.Problems) != 1 {
		t.Fatalf("signed out = %+v", bad)
	}
	p := bad.Problems[0]
	if p.ID != "observer_auth" || p.Severity != "error" || !strings.Contains(p.Text, "2 waiting") ||
		!strings.Contains(p.Fix, "claude") || !strings.Contains(p.Fix, "/login") {
		t.Errorf("problem = %+v", p)
	}
}

func TestMemdAPICost(t *testing.T) {
	db, _ := openTestMemd(t)
	seedAPI(t, db)
	var c struct {
		PriceBasis string  `json:"price_basis"`
		Estimated  bool    `json:"estimated"`
		USD        float64 `json:"usd"`
		Tokens     struct {
			Input, CachedInput, Output int64 `json:"-"`
		}
		USDByKind map[string]float64 `json:"usd_by_kind"`
		Rows      []struct {
			Key      string  `json:"key"`
			Memories int     `json:"memories"`
			USD      float64 `json:"usd"`
		} `json:"rows"`
	}
	apiJSON(t, uiHandler(db), "/api/v1/cost?range=30d&group=project", &c)
	// 1M input * $4 + 0.5M cached * $0.20 + 0.1M output * $20 = 4 + 0.1 + 2 = 6.10
	if c.PriceBasis != "opus_equivalent" || !c.Estimated {
		t.Errorf("basis = %q estimated=%v", c.PriceBasis, c.Estimated)
	}
	if d := c.USD - 6.10; d > 1e-9 || d < -1e-9 {
		t.Errorf("usd = %v, want 6.10", c.USD)
	}
	if len(c.Rows) != 1 || c.Rows[0].Key != "proj-a" || c.Rows[0].Memories != 1 {
		t.Errorf("rows = %+v", c.Rows)
	}
	if code, _ := apiGet(t, uiHandler(db), "/api/v1/cost?range=bogus"); code != 400 {
		t.Errorf("bad range = %d, want 400", code)
	}
}

func TestMemdAPIOtherEndpoints(t *testing.T) {
	db, _ := openTestMemd(t)
	seedAPI(t, db)
	h := uiHandler(db)

	var sess struct {
		Items []struct {
			ID          string `json:"id"`
			Project     string `json:"project"`
			ToolUses    int    `json:"tool_uses"`
			PromptCount int    `json:"prompt_count"`
			Summary     struct {
				Request string `json:"request"`
			} `json:"summary"`
		} `json:"items"`
	}
	apiJSON(t, h, "/api/v1/sessions", &sess)
	if len(sess.Items) != 2 {
		t.Fatalf("sessions = %+v", sess)
	}
	var s1 *struct {
		ID          string `json:"id"`
		Project     string `json:"project"`
		ToolUses    int    `json:"tool_uses"`
		PromptCount int    `json:"prompt_count"`
		Summary     struct {
			Request string `json:"request"`
		} `json:"summary"`
	}
	for i := range sess.Items {
		if sess.Items[i].ID == "s1" {
			s1 = &sess.Items[i]
		}
	}
	if s1 == nil || s1.ToolUses != 1 || s1.PromptCount != 1 || s1.Summary.Request != "req text" {
		t.Errorf("s1 = %+v", s1)
	}
	var detail struct {
		Prompts        []struct{ Text string } `json:"prompts"`
		ObservationIDs []string                `json:"observation_ids"`
	}
	apiJSON(t, h, "/api/v1/sessions/s1", &detail)
	if len(detail.Prompts) != 1 || detail.Prompts[0].Text != "do the thing" || len(detail.ObservationIDs) != 5 {
		t.Errorf("detail = %+v", detail)
	}
	if code, _ := apiGet(t, h, "/api/v1/sessions/none"); code != 404 {
		t.Errorf("missing session = %d", code)
	}

	var ws struct {
		Lists []struct {
			Name  string           `json:"name"`
			Items []map[string]any `json:"items"`
		} `json:"lists"`
	}
	apiJSON(t, h, "/api/v1/work-state?project=proj-a", &ws)
	names := map[string]bool{}
	for _, l := range ws.Lists {
		names[l.Name] = len(l.Items) > 0
	}
	if !names["todo"] || !names["facts"] {
		t.Errorf("work-state lists = %+v", ws)
	}

	var dl struct {
		Items []struct{ Title, Location string } `json:"items"`
	}
	apiJSON(t, h, "/api/v1/deliverables", &dl)
	if len(dl.Items) != 1 || dl.Items[0].Title != "report.md" || dl.Items[0].Location != "/x/report.md" {
		t.Errorf("deliverables = %+v", dl)
	}

	var ob struct {
		Queue      map[string]int `json:"queue"`
		Rejections []struct {
			Reason string
			Count  int
		} `json:"rejections"`
	}
	apiJSON(t, h, "/api/v1/observer", &ob)
	if ob.Queue["queued"] != 1 || ob.Queue["done"] != 1 || ob.Queue["rejected"] != 1 ||
		len(ob.Rejections) != 1 || ob.Rejections[0].Reason != "schema" {
		t.Errorf("observer = %+v", ob)
	}

	var pr struct {
		Raw  bool   `json:"raw_tool_uses_enabled"`
		Bind string `json:"bind"`
	}
	apiJSON(t, h, "/api/v1/privacy", &pr)
	if pr.Raw || pr.Bind != "127.0.0.1:37800" {
		t.Errorf("privacy = %+v", pr)
	}

	var st struct {
		Totals map[string]int `json:"totals"`
	}
	apiJSON(t, h, "/api/v1/stats", &st)
	if st.Totals["observations"] != 5 || st.Totals["raw_tool_uses"] != 1 {
		t.Errorf("stats = %+v", st)
	}
	var pj struct {
		Items []struct {
			Name  string `json:"name"`
			Count int    `json:"count"`
		} `json:"items"`
	}
	apiJSON(t, h, "/api/v1/projects", &pj)
	if len(pj.Items) == 0 || pj.Items[0].Name != "proj-a" || pj.Items[0].Count != 5 {
		t.Errorf("projects = %+v", pj)
	}
}

func TestMemdAPIEvents(t *testing.T) {
	db, _ := openTestMemd(t)
	old := uiTick
	uiTick = 50 * time.Millisecond
	t.Cleanup(func() { uiTick = old })

	srv := httptest.NewServer(uiHandler(db))
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	readEvent := func() (string, string) {
		var ev, data string
		for sc.Scan() {
			l := sc.Text()
			switch {
			case strings.HasPrefix(l, "event: "):
				ev = strings.TrimPrefix(l, "event: ")
			case strings.HasPrefix(l, "data: "):
				data = strings.TrimPrefix(l, "data: ")
			case l == "" && ev != "":
				return ev, data
			}
		}
		return "", ""
	}
	if ev, data := readEvent(); ev != "health" || !json.Valid([]byte(data)) {
		t.Fatalf("first event = %q %q, want health", ev, data)
	}
	if ev, _ := readEvent(); ev != "counters" {
		t.Errorf("second event = %q, want counters", ev)
	}
	apiExec(t, db, `INSERT INTO observations (project, type, title, source) VALUES ('p','bugfix','t','live')`)
	deadline := time.After(3 * time.Second)
	got := make(chan string, 1)
	go func() {
		for {
			ev, data := readEvent()
			if ev == "" {
				got <- ""
				return
			}
			if ev == "invalidate" {
				got <- data
				return
			}
		}
	}()
	select {
	case d := <-got:
		if !strings.Contains(d, "memory") {
			t.Errorf("invalidate data = %q", d)
		}
	case <-deadline:
		t.Fatal("no invalidate after insert")
	}

	// Client disconnect must end the handler: srv.Close waits for it.
	cancel()
	resp.Body.Close()
	closed := make(chan struct{})
	go func() { srv.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("handler still running after client disconnect")
	}
}

// ---- viewer endpoints: filters, facets, timeseries, system, handoffs ----

func seedUIDays(t *testing.T, db *sql.DB) {
	t.Helper()
	apiExec(t, db, `INSERT INTO sessions (session_id, project) VALUES ('sess-one','proj-a'), ('sess-two','proj-b')`)
	ins := `INSERT INTO observations (project, session_id, type, raw_type, title, source, created_at)
		VALUES (?, ?, ?, ?, ?, 'live', CAST(? AS TIMESTAMP))`
	apiExec(t, db, ins, "proj-a", "sess-one", "bugfix", "bugfix", "old a", "2026-01-10 10:00:00")
	apiExec(t, db, ins, "proj-a", "sess-one", "feature", "feature", "mid a", "2026-01-11 23:30:00")
	apiExec(t, db, ins, "proj-b", "sess-two", "bugfix", "bugfix", "new b", "2026-01-12 08:00:00")
	apiExec(t, db, ins, "proj-b", "sess-two", "bugfix", "bugfix", "new b2", "2026-01-12 09:00:00")
}

func TestMemdAPIObservationFilters(t *testing.T) {
	db, _ := openTestMemd(t)
	seedUIDays(t, db)
	h := uiHandler(db)
	titles := func(q string) []string {
		var p struct {
			Items []struct{ Title string } `json:"items"`
			Total int                      `json:"total"`
		}
		apiJSON(t, h, "/api/v1/observations?"+q, &p)
		var out []string
		for _, i := range p.Items {
			out = append(out, i.Title)
		}
		if p.Total != len(out) {
			t.Errorf("%s: total %d != items %d", q, p.Total, len(out))
		}
		return out
	}
	tests := []struct {
		q    string
		want string
	}{
		{"session=sess-two", "new b2,new b"},
		{"from=2026-01-11&to=2026-01-11", "mid a"},
		{"from=2026-01-12", "new b2,new b"},
		{"to=2026-01-10", "old a"},
		// UTC+2: 23:30Z on the 11th is 01:30 local on the 12th.
		{"from=2026-01-12&to=2026-01-12&tz_offset=120", "new b2,new b,mid a"},
		{"session=sess-two&type=feature", ""},
	}
	for _, tt := range tests {
		if got := strings.Join(titles(tt.q), ","); got != tt.want {
			t.Errorf("%s = %q, want %q", tt.q, got, tt.want)
		}
	}
	for _, bad := range []string{"from=yesterday", "to=2026-13-40", "from=2026-01-01&tz_offset=abc", "tz_offset=99999"} {
		if code, _ := apiGet(t, h, "/api/v1/observations?"+bad); code != 400 {
			t.Errorf("%s = %d, want 400", bad, code)
		}
	}
}

func TestMemdAPIFacets(t *testing.T) {
	db, _ := openTestMemd(t)
	seedUIDays(t, db)
	var f struct {
		Projects []struct {
			Name  string
			Count int
		}
		Types []struct {
			Name  string
			Count int
		}
		FirstAt string `json:"first_at"`
		LastAt  string `json:"last_at"`
	}
	apiJSON(t, uiHandler(db), "/api/v1/facets", &f)
	if len(f.Projects) != 2 || f.Projects[0].Name != "proj-a" || f.Projects[0].Count != 2 {
		t.Errorf("projects = %+v", f.Projects)
	}
	if len(f.Types) != 2 || f.Types[0].Name != "bugfix" || f.Types[0].Count != 3 {
		t.Errorf("types = %+v", f.Types)
	}
	if f.FirstAt != "2026-01-10T10:00:00Z" || f.LastAt != "2026-01-12T09:00:00Z" {
		t.Errorf("range = %q .. %q", f.FirstAt, f.LastAt)
	}
}

func TestMemdAPITimeseries(t *testing.T) {
	db, _ := openTestMemd(t)
	seedUIDays(t, db)
	h := uiHandler(db)
	type resp struct {
		Items []struct {
			Day, Project, Type string
			Count              int
		} `json:"items"`
	}
	flat := func(r resp) string {
		var s []string
		for _, i := range r.Items {
			s = append(s, fmt.Sprintf("%s/%s/%s=%d", i.Day, i.Project, i.Type, i.Count))
		}
		return strings.Join(s, " ")
	}
	var r resp
	apiJSON(t, h, "/api/v1/timeseries?from=2026-01-01&to=2026-01-31", &r)
	want := "2026-01-10/proj-a/bugfix=1 2026-01-11/proj-a/feature=1 2026-01-12/proj-b/bugfix=2"
	if got := flat(r); got != want {
		t.Errorf("utc = %q, want %q", got, want)
	}
	apiJSON(t, h, "/api/v1/timeseries?from=2026-01-01&to=2026-01-31&tz_offset=120", &r)
	want = "2026-01-10/proj-a/bugfix=1 2026-01-12/proj-a/feature=1 2026-01-12/proj-b/bugfix=2"
	if got := flat(r); got != want {
		t.Errorf("tz=+120 = %q, want %q", got, want)
	}
	apiJSON(t, h, "/api/v1/timeseries?from=2026-01-12&to=2026-01-12&project=proj-b&type=bugfix", &r)
	if got := flat(r); got != "2026-01-12/proj-b/bugfix=2" {
		t.Errorf("filtered = %q", got)
	}
	if code, _ := apiGet(t, h, "/api/v1/timeseries?from=nope"); code != 400 {
		t.Errorf("bad from = %d", code)
	}
}

func TestMemdAPISystem(t *testing.T) {
	db, dir := openTestMemd(t)
	var empty struct {
		Version      string `json:"version"`
		Status       string `json:"status"`
		LastIngestAt string `json:"last_ingest_at"`
		DBBytes      int64  `json:"db_bytes"`
	}
	h := uiHandler(db)
	apiJSON(t, h, "/api/v1/system", &empty)
	if empty.Version == "" || empty.Status != "ok" || empty.LastIngestAt != "" {
		t.Errorf("empty db = %+v", empty)
	}
	if empty.DBBytes <= 0 {
		t.Errorf("db_bytes = %d for %s", empty.DBBytes, dir)
	}
	apiExec(t, db, `INSERT INTO tool_uses (session_id, tool_use_id, tool_name, created_at) VALUES ('s','t1','Bash', TIMESTAMP '2026-02-03 04:05:06')`)
	apiExec(t, db, `INSERT INTO prompts (session_id, prompt_number, text, created_at) VALUES ('s',1,'x', TIMESTAMP '2026-02-03 04:06:00')`)
	apiExec(t, db, `INSERT INTO observations (project, session_id, type, source, created_at) VALUES ('p','s','bugfix','live', TIMESTAMP '2026-02-03 05:00:00')`)
	var s struct {
		LastIngestAt      string `json:"last_ingest_at"`
		LastObservationAt string `json:"last_observation_at"`
		Totals            struct{ Observations, Sessions, Prompts int }
	}
	apiJSON(t, h, "/api/v1/system", &s)
	if s.LastIngestAt != "2026-02-03T04:06:00Z" || s.LastObservationAt != "2026-02-03T05:00:00Z" {
		t.Errorf("times = %+v", s)
	}
	if s.Totals.Observations != 1 || s.Totals.Prompts != 1 {
		t.Errorf("totals = %+v", s.Totals)
	}
}

func TestMemdAPIHandoffs(t *testing.T) {
	db, _ := openTestMemd(t)
	apiExec(t, db, `INSERT INTO sessions (session_id, project) VALUES ('abcd1234-0000-4000-8000-000000000000','proj-a'), ('ffff0000-1111-4000-8000-000000000000','proj-a')`)
	h := uiHandler(db)
	ws := func(project, list, fields, at string) {
		apiExec(t, db, `INSERT INTO work_state (project, list_name, fields, created_at) VALUES (?, ?, ?, CAST(? AS TIMESTAMP))`, project, list, fields, at)
	}
	ws("proj-a", "handoff", `{"text":"# First\nold","handoff_file":"HANDOFF-abcd1234.md"}`, "2026-03-01 10:00:00")
	ws("proj-a", "handoff", `{"consumed_at":1772362800000,"consumed_by":"startup"}`, "2026-03-01 11:00:00")
	ws("proj-a", "handoff", `{"text":"<script>alert(1)</script> second","session_id":"ffff0000-1111-4000-8000-000000000000"}`, "2026-03-02 10:00:00")
	ws("proj-b", "handoff", `{"text":"third, no link","handoff_file":"HANDOFF.md"}`, "2026-03-03 10:00:00")
	ws("proj-a", "todo", `{"task":"not a handoff"}`, "2026-03-03 11:00:00")

	var p struct {
		Items []struct {
			Project     string `json:"project"`
			Text        string `json:"text"`
			CreatedAt   string `json:"created_at"`
			File        string `json:"file"`
			SessionID   string `json:"session_id"`
			SessionLink string `json:"session_link"`
			State       string `json:"state"`
			ConsumedBy  string `json:"consumed_by"`
			ConsumedAt  string `json:"consumed_at"`
		} `json:"items"`
	}
	apiJSON(t, h, "/api/v1/handoffs", &p)
	if len(p.Items) != 3 {
		t.Fatalf("items = %d: %+v", len(p.Items), p.Items)
	}
	third, second, first := p.Items[0], p.Items[1], p.Items[2] // newest first
	if third.Project != "proj-b" || third.SessionID != "" || third.State != "unshown" {
		t.Errorf("third = %+v", third)
	}
	if second.SessionID != "ffff0000-1111-4000-8000-000000000000" || second.SessionLink != "field" || second.State != "unshown" {
		t.Errorf("second = %+v", second)
	}
	if !strings.Contains(second.Text, "<script>") {
		t.Errorf("text must be served verbatim as data, got %q", second.Text)
	}
	if first.SessionID != "abcd1234-0000-4000-8000-000000000000" || first.SessionLink != "filename" ||
		first.State != "shown" || first.ConsumedBy != "startup" || first.ConsumedAt != "2026-03-01T11:00:00Z" {
		t.Errorf("first = %+v", first)
	}
	var one struct{ Items []struct{ Project string } }
	apiJSON(t, h, "/api/v1/handoffs?project=proj-b&limit=5", &one)
	if len(one.Items) != 1 || one.Items[0].Project != "proj-b" {
		t.Errorf("project filter = %+v", one)
	}
	if code, _ := apiGet(t, h, "/api/v1/handoffs?limit=0"); code != 400 {
		t.Errorf("limit=0 = %d", code)
	}
}
