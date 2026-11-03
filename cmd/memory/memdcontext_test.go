package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

// All fixtures are synthetic.

var ctxNow = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

func ctxDB(t *testing.T) *sql.DB {
	t.Helper()
	old := memdQueryTZ
	memdQueryTZ = time.UTC
	t.Cleanup(func() { memdQueryTZ = old })
	db, _ := openTestMemd(t)
	return db
}

func ctxExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func ctxObs(t *testing.T, db *sql.DB, id int, proj, title, narr string, ts time.Time) {
	t.Helper()
	ctxExec(t, db, `INSERT INTO observations (id, project, session_id, type, title, narrative, source, created_at)
		VALUES (?,?,'s','discovery',?,?,'live', CAST(? AS TIMESTAMP))`, id, proj, title, narr, ts.Format("2006-01-02 15:04:05"))
}

func ctxSum(t *testing.T, db *sql.DB, id int, proj, req, learned string, ts time.Time) {
	t.Helper()
	ctxExec(t, db, `INSERT INTO summaries (id, project, session_id, request, learned, source, created_at)
		VALUES (?,?,'s',?,?,'live', CAST(? AS TIMESTAMP))`, id, proj, req, learned, ts.Format("2006-01-02 15:04:05"))
}

func ctxWS(t *testing.T, db *sql.DB, project, list string, row map[string]any) {
	t.Helper()
	if err := workStateAppend(db, project, list, row); err != nil {
		t.Fatalf("workStateAppend: %v", err)
	}
}

var (
	ctxObsLine = regexp.MustCompile(`(?m)^\d+ (\d+:\d+[ap]|") \S+ `)
	ctxSumLine = regexp.MustCompile(`(?m)^S\d+ `)
)

func TestMemdContextJSONShape(t *testing.T) {
	block := "a <b> & \"c\"\nline2"
	raw := contextJSON(block)
	var got struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("invalid JSON %q: %v", raw, err)
	}
	if got.HookSpecificOutput.HookEventName != "SessionStart" || got.HookSpecificOutput.AdditionalContext != block {
		t.Errorf("got %+v", got)
	}
	if !strings.Contains(string(raw), "<b> & ") || strings.HasSuffix(string(raw), "\n") {
		t.Errorf("JSON must not HTML-escape or end in a newline: %q", raw)
	}
}

func TestMemdContextEmptyDB(t *testing.T) {
	db := ctxDB(t)
	block := buildContext(db, "p", ctxNow)
	if !strings.Contains(block, "# Work state: to-do lists and notes that persist across sessions") ||
		!strings.Contains(block, "No open items yet.") {
		t.Errorf("empty block missing work state header or placeholder:\n%s", block)
	}
	if n := ctxSize(block); n > 1500 || n == 0 {
		t.Errorf("empty block JSON = %d bytes, want small", n)
	}
	if !json.Valid(contextJSON(block)) {
		t.Error("invalid JSON")
	}
}

func TestMemdContextSelectionCounts(t *testing.T) {
	db := ctxDB(t)
	base := time.Date(2026, 3, 9, 8, 0, 0, 0, time.UTC)
	for i := 1; i <= 30; i++ {
		ctxObs(t, db, i, "p", fmt.Sprintf("obs-title-%02d", i), "n", base.Add(time.Duration(i)*time.Minute))
	}
	ctxObs(t, db, 99, "other", "obs-title-other", "n", base.Add(time.Hour))
	for i := 1; i <= 15; i++ {
		ctxSum(t, db, i, "p", fmt.Sprintf("sum-request-%02d", i), "l", base.Add(time.Duration(i)*time.Hour))
	}
	block := buildContext(db, "p", ctxNow)
	if got := len(ctxObsLine.FindAllString(block, -1)); got != 20 {
		t.Errorf("observation rows = %d, want 20\n%s", got, block)
	}
	if got := len(ctxSumLine.FindAllString(block, -1)); got != 10 {
		t.Errorf("session rows = %d, want 10", got)
	}
	for _, want := range []string{"obs-title-30", "obs-title-11", "sum-request-15", "sum-request-06"} {
		if !strings.Contains(block, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, bad := range []string{"obs-title-10", "obs-title-other", "sum-request-05"} {
		if strings.Contains(block, bad) {
			t.Errorf("unexpected %q", bad)
		}
	}
	// oldest first
	if strings.Index(block, "obs-title-11") > strings.Index(block, "obs-title-30") {
		t.Error("timeline not oldest first")
	}
	for _, want := range []string{"# [p] recent context, 2026-03-10 12:00pm UTC", "Each row: ID, time, type marker, title", "### Mar 9, 2026"} {
		if !strings.Contains(block, want) {
			t.Errorf("missing %q in\n%s", want, block)
		}
	}
}

func TestMemdContextLastSummary(t *testing.T) {
	db := ctxDB(t)
	base := time.Date(2026, 3, 9, 8, 0, 0, 0, time.UTC)
	ctxObs(t, db, 1, "p", "obs-one", "n", base)
	ctxSum(t, db, 1, "p", "older", "learned-older", base.Add(-time.Hour))
	block := buildContext(db, "p", ctxNow)
	if strings.Contains(block, "**Learned**") {
		t.Error("last summary older than the newest observation must be hidden")
	}
	ctxSum(t, db, 2, "p", "newer", "learned-newer", base.Add(time.Hour))
	block = buildContext(db, "p", ctxNow)
	if !strings.Contains(block, "**Learned**: learned-newer") || strings.Contains(block, "learned-older") {
		t.Errorf("last summary section wrong:\n%s", block)
	}
}

func TestMemdContextWorkState(t *testing.T) {
	db := ctxDB(t)
	now := time.Now()
	ctxWS(t, db, "p", "build", map[string]any{"task": "alpha-task", "status": "doing", "detail": "hidden-detail"})
	ctxWS(t, db, "p", "build", map[string]any{"task": "beta-task", "status": "todo"})
	ctxWS(t, db, "p", "build", map[string]any{"task": "gamma-task", "status": "done"})
	ctxWS(t, db, "p", "build", map[string]any{"owner": "someone"})
	ctxWS(t, db, "p", "facts", map[string]any{"task": "ordinary-key", "color": "blue"})
	ctxWS(t, db, "p", "facts", map[string]any{"shared": "from-project"})
	ctxWS(t, db, "other", "user-facts", map[string]any{"shared": "from-user", "units": "metric"})
	ctxWS(t, db, "p", "other-list", map[string]any{"task": "x", "status": "done"})
	block := buildContext(db, "p", now)
	for _, want := range []string{
		"Still open:", "- build: owner=someone, updated", "  - [doing] alpha-task, updated", "  - [todo] beta-task, updated",
		"# Project facts (durable)", "- color: blue", "- shared: from-project",
		"# Facts (all projects)", "- units: metric",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("missing %q in\n%s", want, block)
		}
	}
	for _, bad := range []string{"hidden-detail", "gamma-task", "from-user", "other-list", "- task:"} {
		if strings.Contains(block, bad) {
			t.Errorf("unexpected %q in\n%s", bad, block)
		}
	}
	// durable lists never show as to-do lists
	if strings.Contains(block, "- facts") || strings.Contains(block, "- user-facts") {
		t.Errorf("durable list rendered as a to-do list:\n%s", block)
	}
}

func TestMemdContextStaleLists(t *testing.T) {
	db := ctxDB(t)
	at := func(d int) string { return ctxNow.AddDate(0, 0, -d).Format("2006-01-02 15:04:05") }
	ins := func(list string, ago int, fields string) {
		ctxExec(t, db, `INSERT INTO work_state (project, list_name, fields, created_at) VALUES ('p', ?, ?, CAST(? AS TIMESTAMP))`, list, fields, at(ago))
	}
	ins("fresh", 1, `{"task":"a","status":"todo"}`)
	ins("stale", 30, `{"task":"b","status":"todo"}`)
	ins("stale", 30, `{"task":"c","status":"doing"}`)
	ins("ancient", 100, `{"task":"d","status":"todo"}`)
	block := buildContext(db, "p", ctxNow)
	for _, want := range []string{"- fresh", "- stale: 2 open, stale 30d", "- ...1 list idle for 90+ days and hidden; work_state_read lists them"} {
		if !strings.Contains(block, want) {
			t.Errorf("missing %q in\n%s", want, block)
		}
	}
	if strings.Contains(block, "ancient") {
		t.Error("90+ day list must be counted, not named")
	}
}

func seedHeavy(t *testing.T, db *sql.DB) {
	t.Helper()
	long := strings.Repeat("word ", 120)
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	ctxExec(t, db, `INSERT INTO observations (id, project, session_id, type, title, narrative, source, created_at)
		SELECT i, 'p', 's', 'discovery', 'Observation ' || lpad(CAST(i AS VARCHAR), 3, '0') || ' ' || ?, repeat('narrative ', 400), 'live',
		CAST(? AS TIMESTAMP) + i * INTERVAL 1 MINUTE FROM range(1, 501) t(i)`, long, base.Format("2006-01-02 15:04:05"))
	ctxExec(t, db, `INSERT INTO summaries (id, project, session_id, request, learned, source, created_at)
		SELECT i, 'p', 's', 'Request ' || lpad(CAST(i AS VARCHAR), 2, '0') || ' ' || ?, repeat('learned ', 300), 'live',
		CAST(? AS TIMESTAMP) + i * INTERVAL 1 HOUR FROM range(1, 21) t(i)`, long, base.Format("2006-01-02 15:04:05"))
	for i := 0; i < 12; i++ {
		ctxWS(t, db, "p", "facts", map[string]any{fmt.Sprintf("fact-%02d", i): strings.Repeat("f", 300)})
		ctxWS(t, db, "p", "user-facts", map[string]any{fmt.Sprintf("ufact-%02d", i): strings.Repeat("u", 300)})
	}
	ctxWS(t, db, "p", "build", map[string]any{"task": "keep-me-open", "status": "doing"})
}

func TestMemdContextBudget500(t *testing.T) {
	db := ctxDB(t)
	seedHeavy(t, db)
	block := buildContext(db, "p", ctxNow)
	n := ctxSize(block)
	t.Logf("500-observation JSON bytes = %d (target %d, guard %d)", n, contextTarget, contextGuard)
	if n > contextGuard {
		t.Fatalf("JSON = %d bytes, over guard %d", n, contextGuard)
	}
	if n > contextTarget {
		t.Errorf("JSON = %d bytes, over target %d though trimming was possible", n, contextTarget)
	}
	if n < contextTarget-500 {
		t.Errorf("JSON = %d bytes, trimmed far below target %d", n, contextTarget)
	}
	if !strings.Contains(block, "keep-me-open") {
		t.Error("open work_state item missing")
	}
	// oldest observations go first: the newest one survives, the oldest does not
	if !strings.Contains(block, "Observation 500") || strings.Contains(block, "Observation 481 ") {
		t.Error("trim must drop oldest observations first")
	}
}

func TestMemdContextOversizedOpenItems(t *testing.T) {
	db := ctxDB(t)
	seedHeavy(t, db)
	for l := 0; l < 40; l++ {
		for i := 0; i < 20; i++ {
			ctxWS(t, db, "p", fmt.Sprintf("list-%02d", l), map[string]any{
				"task": fmt.Sprintf("task-%02d-%02d %s", l, i, strings.Repeat("x", 200)), "status": "todo"})
		}
	}
	block := buildContext(db, "p", ctxNow)
	n := ctxSize(block)
	t.Logf("oversized open items JSON bytes = %d", n)
	if n > contextGuard {
		t.Fatalf("JSON = %d bytes, over guard %d", n, contextGuard)
	}
	if !strings.Contains(block, "Still open:") || !strings.Contains(block, "more line") {
		t.Error("open items should be present and truncated with a pointer")
	}
	if !strings.Contains(block, "  - [todo] task-39-00 ") {
		t.Errorf("first open item line must survive:\n%s", block)
	}
}

func TestMemdContextGiantInputs(t *testing.T) {
	cases := []struct {
		name string
		seed func(t *testing.T, db *sql.DB)
	}{
		{"giant observation title", func(t *testing.T, db *sql.DB) {
			ctxObs(t, db, 1, "p", strings.Repeat("A", 2<<20), "n", ctxNow.Add(-time.Hour))
		}},
		{"giant summary fields", func(t *testing.T, db *sql.DB) {
			ctxObs(t, db, 1, "p", "o", "n", ctxNow.Add(-2*time.Hour))
			ctxSum(t, db, 1, "p", strings.Repeat("R", 1<<20), strings.Repeat("L", 1<<20), ctxNow.Add(-time.Hour))
		}},
		{"giant work_state values", func(t *testing.T, db *sql.DB) {
			ctxWS(t, db, "p", "facts", map[string]any{"k": strings.Repeat("v", 1<<20)})
			ctxWS(t, db, "p", "user-facts", map[string]any{"k2": strings.Repeat("v", 1<<20)})
			ctxWS(t, db, "p", "build", map[string]any{"owner": strings.Repeat("o", 1<<20)})
			ctxWS(t, db, "p", "build", map[string]any{"task": strings.Repeat("t", 1<<20), "status": "todo"})
		}},
		{"control-char work_state forces the shrink path", func(t *testing.T, db *sql.DB) {
			ctl := strings.Repeat("\x01", 230)
			for i := 0; i < 8; i++ {
				ctxWS(t, db, "p", "facts", map[string]any{fmt.Sprintf("f%d", i): ctl})
				ctxWS(t, db, "p", "user-facts", map[string]any{fmt.Sprintf("u%d", i): ctl})
				ctxWS(t, db, "p", "build", map[string]any{"task": "t" + strings.Repeat("\x02", 150) + fmt.Sprint(i), "status": "todo"})
			}
			ctxObs(t, db, 1, "p", "o", "n", ctxNow.Add(-time.Hour))
		}},
		{"escape-heavy multibyte", func(t *testing.T, db *sql.DB) {
			bad := strings.Repeat("\"\\\n\x01界😀", 3000)
			for i := 1; i <= 25; i++ {
				ctxObs(t, db, i, "p", bad, "n", ctxNow.Add(-time.Duration(i)*time.Minute))
			}
			ctxWS(t, db, "p", "facts", map[string]any{"k": bad})
			ctxWS(t, db, "p", "build", map[string]any{"task": bad, "status": "todo"})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := ctxDB(t)
			c.seed(t, db)
			block := buildContext(db, "p", ctxNow)
			raw := contextJSON(block)
			t.Logf("JSON bytes = %d", len(raw))
			if len(raw) > contextGuard {
				t.Fatalf("JSON = %d bytes, over guard %d", len(raw), contextGuard)
			}
			if !json.Valid(raw) {
				t.Fatal("invalid JSON")
			}
		})
	}
}

func TestMemdContextCutFallback(t *testing.T) {
	got := ctxCut(strings.Repeat("\x01界", 20000))
	if n := ctxSize(got); n > contextGuard || n < contextGuard-30 {
		t.Errorf("cut JSON = %d bytes, want just under %d", n, contextGuard)
	}
	if !strings.HasSuffix(got, ctxCutMarker) {
		t.Error("cut text must end with the marker")
	}
}

func TestMemdContextGiantTaskKeepsItem(t *testing.T) {
	db := ctxDB(t)
	ctxWS(t, db, "p", "build", map[string]any{"task": strings.Repeat("T", 1<<20), "status": "todo"})
	ctxWS(t, db, "p", "build", map[string]any{"task": "second-task", "status": "todo"})
	block := buildContext(db, "p", time.Now())
	if n := ctxSize(block); n > contextGuard {
		t.Fatalf("JSON = %d bytes, over guard", n)
	}
	if !strings.Contains(block, "  - [todo] TTTT") || !strings.Contains(block, "…") {
		t.Errorf("giant task must render clipped, not vanish:\n%s", block)
	}
	if !strings.Contains(block, "  - [todo] second-task, updated") {
		t.Errorf("later items must still render when the clipped first one fits:\n%s", block)
	}
}

func TestMemdContextShrinkPathKeepsFirstItem(t *testing.T) {
	wd := &workData{open: []string{"- list: s, updated 1 minute ago", "  - [todo] first-item, updated 1 minute ago", "  - [todo] second-item, updated 1 minute ago"}}
	got := wd.render(0)
	if !strings.Contains(got, "- list: s, updated 1 minute ago") || strings.Contains(got, "second-item") ||
		!strings.Contains(got, "- ...2 more lines; work_state_read shows them") {
		t.Errorf("scale 0 must keep the first line and a pointer:\n%s", got)
	}
}

func TestMemdContextWorkStateReadError(t *testing.T) {
	db := ctxDB(t)
	ctxExec(t, db, `INSERT INTO work_state (project, list_name, fields) VALUES ('p', 'build', 'not json')`)
	block := buildContext(db, "p", ctxNow)
	if !strings.Contains(block, "Work state unavailable (read error); try work_state_read") {
		t.Errorf("read error must be stated:\n%s", block)
	}
	if strings.Contains(block, "No open items yet.") {
		t.Error("must not claim nothing is open when the read failed")
	}
}
