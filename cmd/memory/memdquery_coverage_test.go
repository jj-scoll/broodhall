package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestMemdQueryTimelineAnchors(t *testing.T) {
	db := queryFixture(t)
	d11 := map[string]any{"depth_before": float64(1), "depth_after": float64(1)}
	with := func(extra map[string]any) map[string]any {
		m := map[string]any{}
		for k, v := range d11 {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	tests := []struct {
		name string
		args map[string]any
		in   []string
		out  []string
		eq   string // exact output when set
	}{
		{
			name: "observation id as numeric string with project",
			args: with(map[string]any{"anchor": "2", "project": "proj-a"}),
			in:   []string{"# Timeline around anchor: 2\n", "**Window:** 1 before, 1 after | **Items:** 3"},
			out:  []string{"#4 |", "#1 |"},
		},
		{
			name: "observation id in another project is not found",
			args: with(map[string]any{"anchor": float64(3), "project": "proj-a"}),
			eq:   "Observation #3 not found",
		},
		{
			name: "summary anchor with hash and lowercase s",
			args: with(map[string]any{"anchor": "#s1"}),
			in:   []string{"# Timeline around anchor: S1\n", "<- **ANCHOR**"},
		},
		{
			name: "summary anchor wrong project",
			args: with(map[string]any{"anchor": "S1", "project": "proj-b"}),
			eq:   "Session #1 not found",
		},
		{name: "summary anchor missing", args: with(map[string]any{"anchor": "S999"}), eq: "Session #999 not found"},
		{
			name: "summary anchor zero",
			args: with(map[string]any{"anchor": "S0"}),
			eq:   "A session anchor looks like S12: the letter S and a positive whole number",
		},
		{
			name: "summary anchor with junk",
			args: with(map[string]any{"anchor": "Sabc"}),
			eq:   "A session anchor looks like S12: the letter S and a positive whole number",
		},
		{
			name: "timestamp anchor splits strictly by time",
			args: with(map[string]any{"anchor": "2026-03-02T09:10:00"}),
			in:   []string{"# Timeline around anchor: 2026-03-02T09:10:00\n", "**Items:** 3", "#2 |", "#S1", "#4 |"},
			out:  []string{"#1 |"},
		},
		{
			name: "timestamp anchor with space separator",
			args: with(map[string]any{"anchor": "2026-03-02 09:10:00", "project": "proj-a"}),
			in:   []string{"# Timeline around anchor: 2026-03-02 09:10:00\n"},
		},
		{name: "invalid timestamp", args: with(map[string]any{"anchor": "yesterday"}), eq: "Invalid timestamp: yesterday"},
		{
			name: "timestamp with nothing around it",
			args: map[string]any{"anchor": "2020-01-01", "depth_before": float64(1), "depth_after": float64(1), "project": "proj-nothing"},
			eq:   "Nothing found around that anchor (1 before, 1 after)",
		},
		{name: "negative numeric anchor", args: with(map[string]any{"anchor": float64(-4)}), eq: "anchor must be a positive observation ID"},
		{name: "bool anchor", args: with(map[string]any{"anchor": true}), eq: "anchor must be a positive observation ID"},
		{name: "fractional anchor", args: with(map[string]any{"anchor": 1.5}), eq: "anchor must be a positive observation ID"},
		{name: "blank depth string", args: map[string]any{"anchor": float64(1), "depth_after": "  "}, eq: "depth_before and depth_after must each be a whole number, 0 or higher"},
		{name: "text depth", args: map[string]any{"anchor": float64(1), "depth_after": "many"}, eq: "depth_before and depth_after must each be a whole number, 0 or higher"},
		{
			name: "query anchor scoped to project",
			args: with(map[string]any{"query": "parser", "project": "proj-a"}),
			in:   []string{"# Timeline for query: \"parser\"\n**Anchor:** Observation #", "**Window:** 1 before, 1 after"},
		},
		{
			name: "query anchor with no hit in the requested project",
			args: with(map[string]any{"query": "export", "project": "proj-a"}),
			eq:   `Nothing in memory matches "export". Try other search words.`,
		},
		{
			name: "date filters are ignored for the anchor",
			args: with(map[string]any{"anchor": float64(2), "dateStart": "2030-01-01"}),
			in:   []string{"# Timeline around anchor: 2\n"},
		},
		{name: "bad date filter is an error text", args: with(map[string]any{"anchor": float64(2), "dateStart": "nope"})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := memTimeline(db, tt.args)
			if tt.name == "bad date filter is an error text" {
				if err == nil || !strings.Contains(err.Error(), `invalid date "nope"`) {
					t.Fatalf("want invalid date error, got %q, %v", got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.eq != "" && got != tt.eq {
				t.Errorf("got %q want %q", got, tt.eq)
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
}

func TestMemdQueryTimelineAnchorContents(t *testing.T) {
	db := queryFixture(t)
	// Timestamp anchor 09:10:00 is the summary's time: that record stays in the window, with
	// one record before it and one after it.
	got, err := memTimeline(db, map[string]any{"anchor": "2026-03-02T09:10:00", "project": "proj-a", "depth_before": float64(1), "depth_after": float64(1)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "#2 |") || !strings.Contains(got, "#4 |") {
		t.Errorf("want nearest neighbours #2 and #4:\n%s", got)
	}
	if strings.Contains(got, "#1 |") {
		t.Errorf("#1 is two records back and must be outside depth 1:\n%s", got)
	}
	// With depth 0 on both sides only the record at the anchor instant (the summary) remains.
	got, _ = memTimeline(db, map[string]any{"anchor": "2026-03-02T09:10:00", "depth_before": float64(0), "depth_after": float64(0)})
	if !strings.Contains(got, "**Items:** 1") || !strings.Contains(got, "#S1") || strings.Contains(got, "#2 |") || strings.Contains(got, "#4 |") {
		t.Errorf("depth 0 timestamp anchor:\n%s", got)
	}
}

func TestMemdQuerySearchArgErrors(t *testing.T) {
	db := queryFixture(t)
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"bad dateStart", map[string]any{"dateStart": "soon"}, `invalid date "soon": use an ISO date or timestamp`},
		{"bad dateEnd", map[string]any{"dateEnd": "2026-13-45"}, `invalid date "2026-13-45": use an ISO date or timestamp`},
		{"text limit", map[string]any{"limit": "lots"}, "limit must be an integer of 0 or more"},
		{"negative limit", map[string]any{"limit": float64(-1)}, "limit must be an integer of 0 or more"},
		{"fractional limit", map[string]any{"limit": 2.5}, "limit must be an integer of 0 or more"},
		{"bool limit", map[string]any{"limit": true}, "limit must be an integer of 0 or more"},
		{"blank limit string", map[string]any{"limit": " "}, "limit must be an integer of 0 or more"},
		{"text offset", map[string]any{"offset": "later"}, "offset must be an integer of 0 or more"},
		{"negative offset", map[string]any{"offset": float64(-3)}, "offset must be an integer of 0 or more"},
		{"unknown source", map[string]any{"source": "everything"}, `source must be "notes" or "raw"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := memSearch(db, tt.args)
			if err == nil {
				t.Fatalf("want error %q, got output %q", tt.want, got)
			}
			if err.Error() != tt.want {
				t.Errorf("error %q want %q", err, tt.want)
			}
			if got != "" {
				t.Errorf("output on error: %q", got)
			}
		})
	}
}

func TestMemdQuerySearchNumericForms(t *testing.T) {
	db := queryFixture(t)
	tests := []struct {
		name string
		args map[string]any
		in   string
	}{
		{"limit as string", map[string]any{"limit": "1"}, "#3 |"},
		{"limit as int", map[string]any{"limit": 1}, "#3 |"},
		{"limit as int64", map[string]any{"limit": int64(1)}, "#3 |"},
		{"limit zero means default", map[string]any{"limit": float64(0)}, "#4 |"},
		{"offset skips newest in each category", map[string]any{"offset": float64(1), "type": "observations"}, "(3 obs, 0 sessions, 0 prompts)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := memSearch(db, tt.args)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(got, tt.in) {
				t.Errorf("missing %q in:\n%s", tt.in, got)
			}
		})
	}
}

func TestMemdQuerySearchTypeFilters(t *testing.T) {
	db := queryFixture(t)
	tests := []struct {
		name string
		args map[string]any
		in   []string
		out  []string
	}{
		{
			name: "all categories",
			args: map[string]any{"type": "all"},
			in:   []string{"#1 |", "#S1 |", "#P1 |", "(4 obs, 1 sessions, 1 prompts)"},
		},
		{
			name: "observations and prompts",
			args: map[string]any{"type": "observations,prompts"},
			in:   []string{"#1 |", "#P1 |", "(4 obs, 0 sessions, 1 prompts)"},
			out:  []string{"#S1"},
		},
		{
			name: "sessions and prompts",
			args: map[string]any{"type": []any{"sessions", "prompts"}},
			in:   []string{"#S1 |", "#P1 |"},
			out:  []string{"#1 |", "#3 |"},
		},
		{
			name: "obs_type wins over categories",
			args: map[string]any{"obs_type": "discovery", "type": "sessions"},
			in:   []string{"#2 |", "(1 obs, 0 sessions, 0 prompts)"},
			out:  []string{"#S1", "#1 |"},
		},
		{
			name: "non-category type names merge with obs_type",
			args: map[string]any{"obs_type": "bugfix", "type": "feature"},
			in:   []string{"#1 |", "#3 |"},
			out:  []string{"#2 |", "#4 |"},
		},
		{
			name: "category plus a type name searches observations of that type only",
			args: map[string]any{"type": "sessions,bugfix"},
			in:   []string{"#1 |"},
			out:  []string{"#S1", "#2 |"},
		},
		{
			name: "type list with spaces and empties",
			args: map[string]any{"obs_type": " bugfix , ,change "},
			in:   []string{"#1 |", "#4 |"},
			out:  []string{"#2 |", "#3 |"},
		},
		{
			name: "type with no rows",
			args: map[string]any{"obs_type": "security"},
			in:   []string{"(no notes matched)"},
			out:  []string{"#1 |"},
		},
		{
			name: "project and type combined",
			args: map[string]any{"project": "proj-b", "obs_type": "bugfix"},
			in:   []string{"(no notes matched)"},
			out:  []string{"#1 |"},
		},
		{
			name: "sessions respect the project filter",
			args: map[string]any{"type": "sessions", "project": "proj-b"},
			in:   []string{"(no notes matched)"},
			out:  []string{"#S1"},
		},
		{
			name: "date_desc puts newest day first",
			args: map[string]any{"orderBy": "date_desc"},
			in:   []string{"### Mar 3, 2026"},
		},
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
	t.Run("date_desc orders days newest first", func(t *testing.T) {
		got, _ := memSearch(db, map[string]any{"orderBy": "date_desc"})
		if strings.Index(got, "### Mar 3, 2026") > strings.Index(got, "### Mar 2, 2026") {
			t.Errorf("Mar 3 should precede Mar 2:\n%s", got)
		}
	})
}

func TestMemdQuerySearchSources(t *testing.T) {
	db := queryFixture(t)
	t.Run("notes only reports no match without raw fallback", func(t *testing.T) {
		got, err := memSearch(db, map[string]any{"query": "zzzqqqnomatch", "source": "notes"})
		if err != nil {
			t.Fatal(err)
		}
		if want := `No results found matching "zzzqqqnomatch"`; got != want {
			t.Errorf("got %q want %q", got, want)
		}
	})
	t.Run("raw source searches session records, not notes", func(t *testing.T) {
		got, err := memSearch(db, map[string]any{"query": "parser", "source": "raw"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(got, `Found 1 session(s) matching "parser" in the raw transcripts`) ||
			!strings.Contains(got, "`s1`") || !strings.Contains(got, "[prompt] please fix the widget parser crash") ||
			strings.Contains(got, "#1 |") {
			t.Errorf("raw search:\n%s", got)
		}
		got, _ = memSearch(db, map[string]any{"query": "zzzqqqnomatch", "source": "raw"})
		if want := `No results found matching "zzzqqqnomatch"`; got != want {
			t.Errorf("raw no match: got %q want %q", got, want)
		}
	})
	t.Run("branch or model forces raw search", func(t *testing.T) {
		for _, args := range []map[string]any{
			{"query": "parser", "branch": "main"},
			{"query": "parser", "model": "some-model"},
		} {
			got, err := memSearch(db, args)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(got, "#1 |") || strings.Contains(got, "| #") {
				t.Errorf("%v should skip the notes:\n%s", args, got)
			}
		}
	})
	t.Run("all behaves like the default", func(t *testing.T) {
		a, _ := memSearch(db, map[string]any{"source": "all"})
		b, _ := memSearch(db, map[string]any{})
		if a != b {
			t.Errorf("source=all differs from default:\n%s\n---\n%s", a, b)
		}
	})
}

func TestMemdQueryDisplayFormatter(t *testing.T) {
	parse := func(t *testing.T, s string) any {
		t.Helper()
		v := fE(s)
		if _, isStr := v.(string); isStr {
			t.Fatalf("fE(%q) stayed text", s)
		}
		return v
	}
	many := make([]string, 25)
	for i := range many {
		many[i] = fmt.Sprint(i)
	}
	manyObj := make([]string, 25)
	for i := range manyObj {
		manyObj[i] = fmt.Sprintf(`"k%d":%d`, i, i)
	}
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"nil", nil, "(not recorded)"},
		{"string", "plain", "plain"},
		{"true", true, "true"},
		{"false", false, "false"},
		{"integer number", json.Number("120"), "120"},
		{"float number", json.Number("1.50"), "1.5"},
		{"exponent number", json.Number("1e3"), "1000"},
		{"unparsable number kept", json.Number("not-a-number"), "not-a-number"},
		{"empty list", []any{}, "(empty list)"},
		{"list", parse(t, `["a",2,true]`), "1: a\n2: 2\n3: true"},
		{"empty object", parse(t, `{}`), "(empty object)"},
		{"object keeps key order", parse(t, `{"z":1,"a":"x","m":null}`), "z: 1\na: x\nm: (not recorded)"},
		{"list truncated at 20", parse(t, "["+strings.Join(many, ",")+"]"), ""},
		{"object truncated at 20", parse(t, "{"+strings.Join(manyObj, ",")+"}"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dE(tt.in, 0)
			switch tt.name {
			case "list truncated at 20", "object truncated at 20":
				const note = "\n[Some fields were left out to keep this view short.]"
				if !strings.HasSuffix(got, note) {
					t.Fatalf("missing truncation note:\n%s", got)
				}
				if n := strings.Count(strings.TrimSuffix(got, note), "\n") + 1; n != 20 {
					t.Errorf("kept %d entries, want 20:\n%s", n, got)
				}
				return
			}
			if got != tt.want {
				t.Errorf("got %q want %q", got, tt.want)
			}
		})
	}
	t.Run("nested values are all rendered, in order", func(t *testing.T) {
		got := dE(parse(t, `{"cmd":"go","opts":{"n":1.5,"tags":["a","b"]}}`), 0)
		last := -1
		for _, s := range []string{"cmd: go", "opts:", "n: 1.5", "tags:", "1: a", "2: b"} {
			i := strings.Index(got, s)
			if i < 0 || i < last {
				t.Fatalf("%q missing or out of order in %q", s, got)
			}
			last = i
		}
	})
	t.Run("nesting past depth 4 is omitted", func(t *testing.T) {
		got := dE(parse(t, `{"a":{"b":{"c":{"d":{"e":1}}}}}`), 0)
		if !strings.Contains(got, "d: [Nested value omitted]") || strings.Contains(got, "e: 1") {
			t.Errorf("got %q", got)
		}
	})
	t.Run("string capped", func(t *testing.T) {
		got := dE(strings.Repeat("x", 20000), 0)
		if len(got) >= 20000 {
			t.Errorf("long string not bounded: %d bytes", len(got))
		}
	})
}

func TestMemdQueryFE(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		isText bool
	}{
		{"plain text", "hello", true},
		{"json scalar stays text", `"quoted"`, true},
		{"object", `{"a":1}`, false},
		{"array with leading space", " \n[1,2]", false},
		{"broken object", `{"a":`, true},
		{"trailing json value", `{"a":1} [2]`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fE(tt.in)
			s, isStr := got.(string)
			if isStr != tt.isText {
				t.Fatalf("fE(%q) = %#v, text=%v want %v", tt.in, got, isStr, tt.isText)
			}
			if tt.isText && s != tt.in {
				t.Errorf("text changed: %q", s)
			}
		})
	}
}

func TestMemdQueryToolUsesStructuredPayloads(t *testing.T) {
	db := queryFixture(t)
	got, err := memGetToolUses(db, map[string]any{"ids": []any{float64(1)}})
	if err != nil {
		t.Fatal(err)
	}
	// Input is JSON: rendered as an outline in stored key order, numbers without decoration.
	if !strings.Contains(got, "Input:\ncommand: go test ./...\ntimeout: 120\n") {
		t.Errorf("structured input outline:\n%s", got)
	}
	if !strings.Contains(got, "Raw tool I/O (1 of 1 shown).") {
		t.Errorf("header:\n%s", got)
	}
}

// A missing table stands in for any storage failure: the tools must return the error, not an
// empty or partial answer.
func TestMemdQueryStorageErrorsPropagate(t *testing.T) {
	d11 := map[string]any{"depth_before": float64(1), "depth_after": float64(1)}
	with := func(k string, v any) map[string]any {
		m := map[string]any{k: v}
		for dk, dv := range d11 {
			m[dk] = dv
		}
		return m
	}
	tests := []struct {
		name string
		drop string
		call func(db *sql.DB) (string, error)
	}{
		{"search observations", "observations", func(db *sql.DB) (string, error) { return memSearch(db, map[string]any{"type": "observations"}) }},
		{"search summaries", "summaries", func(db *sql.DB) (string, error) { return memSearch(db, map[string]any{"type": "sessions"}) }},
		{"search prompts", "prompts", func(db *sql.DB) (string, error) { return memSearch(db, map[string]any{"type": "prompts"}) }},
		{"timeline observation anchor", "observations", func(db *sql.DB) (string, error) { return memTimeline(db, with("anchor", float64(2))) }},
		{"timeline summary anchor", "summaries", func(db *sql.DB) (string, error) { return memTimeline(db, with("anchor", "S1")) }},
		{"timeline query anchor", "observations", func(db *sql.DB) (string, error) { return memTimeline(db, with("query", "parser")) }},
		{"timeline window summaries", "summaries", func(db *sql.DB) (string, error) { return memTimeline(db, with("anchor", float64(2))) }},
		{"timeline window prompts", "prompts", func(db *sql.DB) (string, error) { return memTimeline(db, with("anchor", float64(2))) }},
		{"timeline window files", "observation_files", func(db *sql.DB) (string, error) { return memTimeline(db, with("anchor", float64(2))) }},
		{"timeline timestamp anchor", "observations", func(db *sql.DB) (string, error) { return memTimeline(db, with("anchor", "2026-03-02")) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := queryFixture(t)
			if _, err := db.Exec("DROP TABLE " + tt.drop); err != nil {
				t.Skipf("cannot drop %s in this fixture: %v", tt.drop, err)
			}
			got, err := tt.call(db)
			if err == nil {
				t.Fatalf("want an error after dropping %s, got output:\n%s", tt.drop, got)
			}
			if got != "" {
				t.Errorf("output alongside error: %q", got)
			}
		})
	}
}
