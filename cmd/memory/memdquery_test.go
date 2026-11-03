package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Fixture rows are synthetic. The golden files in testdata/memory/golden_formats follow the
// shape of the search, timeline, get_observations and get_tool_uses output.

func queryFixture(t *testing.T) *sql.DB {
	t.Helper()
	old := memdQueryTZ
	memdQueryTZ = time.UTC
	t.Cleanup(func() { memdQueryTZ = old })
	db, _ := openTestMemd(t)
	ex := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	ex(`INSERT INTO sessions (session_id, project) VALUES ('s1','proj-a'), ('s2','proj-b')`)
	obs := func(id int, proj, sid, typ, title, sub, narr, facts, concepts, ts string) {
		ex(`INSERT INTO observations (id, project, session_id, type, title, subtitle, narrative, facts, concepts, source, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,'live', CAST(? AS TIMESTAMP))`, id, proj, sid, typ, title, sub, narr, facts, concepts, ts)
	}
	obs(1, "proj-a", "s1", "bugfix", "Fix nil deref in widget parser", "parser guard added",
		"Added a nil check in src/widget/parser.go before the token is read.",
		`["token checked for nil","no panic on empty input"]`, `["nil-check","parser"]`, "2026-03-02 09:05:00")
	obs(2, "proj-a", "s1", "discovery", "Lexer tokens are cached", "",
		"The lexer keeps a token cache keyed by offset.", `[]`, `[]`, "2026-03-02 09:05:30")
	obs(3, "proj-b", "s2", "feature", "Add export command", "export writes markdown",
		"New export command writes docs/export.md from the store.", `["export is read only"]`, `["cli"]`, "2026-03-03 14:20:00")
	obs(4, "proj-a", "s1", "change", "Parser fuzz target added", "",
		"Added a fuzz target for the parser entry point.", `[]`, `[]`, "2026-03-02 09:20:00")
	ex(`INSERT INTO observation_files (observation_id, path, kind) VALUES
		(1,'src/widget/parser.go','modified'), (1,'src/widget/lexer.go','read'), (3,'docs/export.md','modified')`)
	ex(`INSERT INTO summaries (id, project, session_id, request, learned, source, created_at)
		VALUES (1,'proj-a','s1','Harden the widget parser','tokens may be nil','live', CAST('2026-03-02 09:10:00' AS TIMESTAMP))`)
	ex(`INSERT INTO prompts (session_id, prompt_number, text, created_at)
		VALUES ('s1',1,'please fix the widget parser crash', CAST('2026-03-02 09:05:10' AS TIMESTAMP))`)
	ex(`INSERT INTO tool_uses (id, session_id, tool_use_id, tool_name, input, output, created_at) VALUES
		(1,'s1','toolu_1','Bash','{"command":"go test ./...","timeout":120}', 'ok  ' || chr(9) || 'pkg' || chr(9) || '0.1s', CAST('2026-03-02 09:05:20' AS TIMESTAMP)),
		(2,'s1','toolu_2','Bash','{"command":"curl -H x","env":"password=hunter2xyz99"}', 'rawtoolcanary', CAST('2026-03-02 09:05:21' AS TIMESTAMP)),
		(3,'s2','toolu_3','Read','{"file_path":"a.go"}', 'other project output', CAST('2026-03-03 14:21:00' AS TIMESTAMP))`)
	return db
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "memory", "golden_formats", name))
	if err != nil {
		t.Fatal(err)
	}
	if got != string(b) {
		t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, b)
	}
}

func TestMemdQueryGolden(t *testing.T) {
	db := queryFixture(t)
	t.Run("search", func(t *testing.T) {
		got, err := memSearch(db, map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		golden(t, "search_all.golden", got)
	})
	t.Run("search prompts opt-in", func(t *testing.T) {
		got, err := memSearch(db, map[string]any{"type": "prompts"})
		if err != nil {
			t.Fatal(err)
		}
		golden(t, "search_prompts.golden", got)
	})
	t.Run("timeline", func(t *testing.T) {
		got, err := memTimeline(db, map[string]any{"anchor": float64(2), "depth_before": float64(5), "depth_after": float64(5)})
		if err != nil {
			t.Fatal(err)
		}
		golden(t, "timeline_anchor.golden", got)
	})
	t.Run("get_observations", func(t *testing.T) {
		got, err := memGetObservations(db, map[string]any{"ids": []any{float64(1), float64(3)}})
		if err != nil {
			t.Fatal(err)
		}
		golden(t, "get_observations.golden", got)
	})
	t.Run("get_tool_uses", func(t *testing.T) {
		got, err := memGetToolUses(db, map[string]any{"ids": []any{float64(1)}})
		if err != nil {
			t.Fatal(err)
		}
		golden(t, "get_tool_uses.golden", got)
	})
}

func TestMemdQuerySearchFilters(t *testing.T) {
	db := queryFixture(t)
	tests := []struct {
		name string
		args map[string]any
		in   []string
		out  []string
	}{
		{"type bugfix via obs_type", map[string]any{"obs_type": "bugfix"}, []string{"#1 |", "(1 obs, 0 sessions, 0 prompts)"}, []string{"#2 |", "#S1"}},
		{"type alias", map[string]any{"type": "feature"}, []string{"#3 |"}, []string{"#1 |", "#S1"}},
		{"obs_type list", map[string]any{"obs_type": "bugfix,change"}, []string{"#1 |", "#4 |"}, []string{"#2 |", "#3 |"}},
		{"project", map[string]any{"project": "proj-b"}, []string{"#3 |"}, []string{"#1 |", "#2 |", "#S1"}},
		{"category sessions", map[string]any{"type": "sessions"}, []string{"#S1 |"}, []string{"#1 |"}},
		{"date range", map[string]any{"dateStart": "2026-03-03", "dateEnd": "2026-03-04"}, []string{"#3 |"}, []string{"#1 |", "#4 |"}},
		{"limit", map[string]any{"limit": float64(1)}, []string{"#3 |", "(4 obs, 1 sessions"}, []string{"#4 |"}},
		{"orderBy date_asc", map[string]any{"orderBy": "date_asc"}, []string{"#1 |"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := memSearch(db, tt.args)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range tt.in {
				if !strings.Contains(got, s) {
					t.Errorf("missing %q in:\n%s", s, got)
				}
			}
			for _, s := range tt.out {
				if strings.Contains(got, s) {
					t.Errorf("unexpected %q in:\n%s", s, got)
				}
			}
		})
	}
	t.Run("orderBy date_asc puts oldest first", func(t *testing.T) {
		got, _ := memSearch(db, map[string]any{"orderBy": "date_asc"})
		if strings.Index(got, "### Mar 2, 2026") > strings.Index(got, "### Mar 3, 2026") {
			t.Errorf("Mar 2 group should precede Mar 3:\n%s", got)
		}
	})
	t.Run("no match", func(t *testing.T) {
		got, err := memSearch(db, map[string]any{"query": "zzzqqqnomatch"})
		if err != nil {
			t.Fatal(err)
		}
		if want := `No results found matching "zzzqqqnomatch"`; got != want {
			t.Errorf("got %q want %q", got, want)
		}
	})
}

func TestMemdQuerySearchRanking(t *testing.T) {
	db := queryFixture(t)
	ex := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	// 10 mentions the exact path once; 11 only shares the token "zap" and is newer.
	ex(`INSERT INTO observations (id, project, session_id, type, title, narrative, source, created_at)
		VALUES (10,'proj-a','s1','discovery','Build layout notes','Entry point lives in internal/zap/qux_handler.go and nowhere else.','live', CAST('2026-03-01 08:00:00' AS TIMESTAMP)),
		       (11,'proj-a','s1','discovery','Handler notes','The handler parser and zap qux are described elsewhere.','live', CAST('2026-03-05 08:00:00' AS TIMESTAMP))`)
	got, err := memSearch(db, map[string]any{"query": "internal/zap/qux_handler.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "#10 |") {
		t.Fatalf("exact path match missing:\n%s", got)
	}
	// 11 shares the token "zap" (OR semantics) and is newer, but 10 holds the whole path.
	i10, i11 := strings.Index(got, "#10 |"), strings.Index(got, "#11 |")
	if i11 < 0 || i10 > i11 {
		t.Errorf("expected #10 before #11 by relevance (11 should still match on zap):\n%s", got)
	}
}

func TestMemdQueryRawToolUsesExcludedFromSearch(t *testing.T) {
	db := queryFixture(t)
	for _, args := range []map[string]any{
		{"query": "rawtoolcanary"},
		{"query": "rawtoolcanary", "type": "all"},
		{},
	} {
		got, err := memSearch(db, args)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(got, "| #") && args["query"] != nil || strings.Contains(got, "toolu_") || strings.Contains(got, "go test") {
			t.Errorf("raw tool use leaked into search %v:\n%s", args, got)
		}
	}
	// Prompts are absent by default, present with the explicit category.
	def, _ := memSearch(db, map[string]any{"query": "crash"})
	if strings.Contains(def, "#P1") {
		t.Errorf("prompt in default search:\n%s", def)
	}
	opt, _ := memSearch(db, map[string]any{"query": "crash", "type": "prompts"})
	if !strings.Contains(opt, "#P1") {
		t.Errorf("prompt missing with type=prompts:\n%s", opt)
	}
}

func TestMemdQueryTimeline(t *testing.T) {
	db := queryFixture(t)
	t.Run("depth window by record count", func(t *testing.T) {
		got, err := memTimeline(db, map[string]any{"anchor": float64(2), "depth_before": float64(1), "depth_after": float64(1)})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "**Window:** 1 records before -> 1 records after | **Items:** 3") {
			t.Errorf("window line:\n%s", got)
		}
		// The window counts every record kind: prompt, anchor, summary. #1 and #4 are out.
		if strings.Contains(got, "#4 |") || strings.Contains(got, "#1 |") || !strings.Contains(got, "User Prompt #1") || !strings.Contains(got, "#S1") {
			t.Errorf("records outside the window:\n%s", got)
		}
	})
	t.Run("query anchor", func(t *testing.T) {
		got, err := memTimeline(db, map[string]any{"query": "export", "depth_before": float64(0), "depth_after": float64(0)})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(got, "# Timeline for query: \"export\"\n**Anchor:** Observation #3 - Add export command\n") {
			t.Errorf("query header:\n%s", got)
		}
	})
	t.Run("session anchor", func(t *testing.T) {
		got, err := memTimeline(db, map[string]any{"anchor": "S1", "depth_before": float64(1), "depth_after": float64(1)})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "**🎯 #S1** Harden the widget parser (Mar 2, 9:10 AM) <- **ANCHOR**") {
			t.Errorf("session anchor:\n%s", got)
		}
	})
	t.Run("errors", func(t *testing.T) {
		tests := []struct {
			name string
			args map[string]any
			want string
		}{
			{"neither", map[string]any{}, `Error: Must provide either "anchor" or "query" parameter`},
			{"both", map[string]any{"anchor": float64(1), "query": "x"}, `Error: Cannot provide both "anchor" and "query" parameters. Use one or the other.`},
			{"missing obs", map[string]any{"anchor": float64(999)}, "Observation #999 not found"},
			{"bad depth", map[string]any{"anchor": float64(1), "depth_before": float64(-1)}, "Invalid timeline depth: depth_before and depth_after must be non-negative safe integers"},
			{"no query hit", map[string]any{"query": "zzzqqqnomatch"}, `No observations found matching "zzzqqqnomatch". Try a different search query.`},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				got, err := memTimeline(db, tt.args)
				if err != nil {
					t.Fatal(err)
				}
				if got != tt.want {
					t.Errorf("got %q want %q", got, tt.want)
				}
			})
		}
	})
}

func TestMemdQueryGetObservations(t *testing.T) {
	db := queryFixture(t)
	got, err := memGetObservations(db, map[string]any{"ids": []any{"3", float64(1)}, "orderBy": "date_asc"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(got, "#1 —") > strings.Index(got, "#3 —") {
		t.Errorf("date_asc order:\n%s", got)
	}
	got, _ = memGetObservations(db, map[string]any{"ids": []any{float64(999)}})
	if got != "No selected observations found." {
		t.Errorf("missing id: %q", got)
	}
	if _, err := memGetObservations(db, map[string]any{}); err == nil {
		t.Error("missing ids must be an error")
	}
	got, _ = memGetObservations(db, map[string]any{"ids": []any{float64(1), float64(3)}, "limit": float64(1)})
	if !strings.HasPrefix(got, "Selected observations (1).") {
		t.Errorf("limit: %q", got)
	}
}

func TestMemdQueryGetToolUses(t *testing.T) {
	db := queryFixture(t)
	t.Run("redacted on serve", func(t *testing.T) {
		got, err := memGetToolUses(db, map[string]any{"ids": []any{"toolu_2"}})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(got, "hunter2xyz99") {
			t.Errorf("secret leaked:\n%s", got)
		}
		if !strings.Contains(got, "Tool #2 — Bash") {
			t.Errorf("row missing:\n%s", got)
		}
	})
	t.Run("project and session filters", func(t *testing.T) {
		got, _ := memGetToolUses(db, map[string]any{"ids": []any{float64(1), float64(3)}, "project": "proj-b"})
		if !strings.Contains(got, "Tool #3") || strings.Contains(got, "Tool #1") {
			t.Errorf("project filter:\n%s", got)
		}
		got, _ = memGetToolUses(db, map[string]any{"ids": []any{float64(1), float64(3)}, "contentSessionId": "s1"})
		if !strings.Contains(got, "Tool #1") || strings.Contains(got, "Tool #3") {
			t.Errorf("session filter:\n%s", got)
		}
	})
	t.Run("none", func(t *testing.T) {
		got, _ := memGetToolUses(db, map[string]any{"ids": []any{float64(99)}})
		if got != "No selected tool calls found." {
			t.Errorf("got %q", got)
		}
	})
	t.Run("ids required", func(t *testing.T) {
		if _, err := memGetToolUses(db, map[string]any{}); err == nil {
			t.Error("want error")
		}
	})
}

func TestMemdQueryPathOnlyInObservationFiles(t *testing.T) {
	db := queryFixture(t)
	if _, err := db.Exec(`INSERT INTO observations (id, project, session_id, type, title, narrative, source, created_at) VALUES
		(20,'proj-a','s1','change','Unrelated words','nothing relevant here','live', CAST('2026-03-01 08:00:00' AS TIMESTAMP)),
		(21,'proj-a','s1','discovery','Chatter','mentions only_in_files.go and zap and internal in passing','live', CAST('2026-03-06 08:00:00' AS TIMESTAMP)),
		(22,'proj-b','s2','change','Other project','also nothing','live', CAST('2026-03-01 09:00:00' AS TIMESTAMP))`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO observation_files (observation_id, path, kind) VALUES
		(20,'internal/zap/only_in_files.go','modified'), (22,'internal/zap/only_in_files.go','read')`); err != nil {
		t.Fatal(err)
	}
	pos := func(got string, ids ...string) []int {
		var p []int
		for _, id := range ids {
			p = append(p, strings.Index(got, "| #"+id+" |"))
		}
		return p
	}
	for _, q := range []string{"internal/zap/only_in_files.go", "zap/only_in_files.go"} {
		got, err := memSearch(db, map[string]any{"query": q, "project": "proj-a"})
		if err != nil {
			t.Fatal(err)
		}
		p := pos(got, "20", "21", "22")
		if p[0] < 0 {
			t.Fatalf("%s: #20 (path only in observation_files) missing:\n%s", q, got)
		}
		if p[2] >= 0 {
			t.Errorf("%s: other project's #22 leaked:\n%s", q, got)
		}
		if p[1] >= 0 && p[0] > p[1] {
			t.Errorf("%s: #20 must rank before the newer fts hit #21:\n%s", q, got)
		}
		if strings.Count(got, "| #20 |") != 1 {
			t.Errorf("%s: #20 duplicated:\n%s", q, got)
		}
	}
	// A '/' boundary is required: a bare suffix of a file name does not match.
	got, _ := memSearch(db, map[string]any{"query": "nly_in_files.go", "project": "proj-a"})
	if strings.Contains(got, "| #20 |") {
		t.Errorf("mid-name suffix matched:\n%s", got)
	}
	// LIKE wildcards in the query are literal.
	got, _ = memSearch(db, map[string]any{"query": "zap/%.go", "project": "proj-a"})
	if strings.Contains(got, "| #20 |") {
		t.Errorf("wildcard matched:\n%s", got)
	}
}
