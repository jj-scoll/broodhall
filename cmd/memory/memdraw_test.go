package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// ---- transcript line builders ----

func rawUser(sess, text, ts, branch string) string {
	return fmt.Sprintf(`{"type":"user","uuid":"u-%s","sessionId":%q,"cwd":"/work/demo","gitBranch":%q,"entrypoint":"cli","timestamp":%q,"message":{"role":"user","content":%q}}`+"\n",
		ts, sess, branch, ts, text)
}

func rawAssistant(sess, uuid, text, ts, model, branch string) string {
	return fmt.Sprintf(`{"type":"assistant","uuid":%q,"sessionId":%q,"cwd":"/work/demo","gitBranch":%q,"entrypoint":"cli","timestamp":%q,"message":{"id":"msg-%s","role":"assistant","model":%q,"stop_reason":"end_turn","content":[{"type":"text","text":%q}]}}`+"\n",
		uuid, sess, branch, ts, uuid, model, text)
}

// rawTool is a tool_use line plus its result line; input is a JSON object.
func rawTool(sess, id, name, input, output, ts, model, branch string) string {
	return fmt.Sprintf(`{"type":"assistant","uuid":"a-%s","sessionId":%q,"cwd":"/work/demo","gitBranch":%q,"entrypoint":"cli","timestamp":%q,"message":{"id":"msg-%s","role":"assistant","model":%q,"stop_reason":"tool_use","content":[{"type":"tool_use","id":%q,"name":%q,"input":%s}]}}`+"\n",
		id, sess, branch, ts, id, model, id, name, input) +
		fmt.Sprintf(`{"type":"user","uuid":"r-%s","sessionId":%q,"cwd":"/work/demo","gitBranch":%q,"entrypoint":"cli","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":%q}]}}`+"\n",
			id, sess, branch, ts, id, output)
}

func rawScanFile(t *testing.T, e *tailEnv, file, body string) {
	t.Helper()
	tailAppend(t, e.path(file), body)
	tailScan(t, e.tailer())
}

func rawIDs(hits []rawHit) []string {
	var out []string
	for _, h := range hits {
		out = append(out, h.SessionID)
	}
	return out
}

func mustRaw(t *testing.T, e *tailEnv, q rawQuery) []rawHit {
	t.Helper()
	hits, err := rawSearch(e.db, q)
	if err != nil {
		t.Fatalf("rawSearch(%+v): %v", q, err)
	}
	return hits
}

// ---- replies and session metadata ----

func TestRawRepliesStoredIdempotent(t *testing.T) {
	body := rawUser("s1", "hello", tailTS(10, 0, 0), "main") +
		rawAssistant("s1", "a1", "first answer", tailTS(10, 0, 1), "model-a", "main") +
		rawAssistant("s1", "a2", "second answer", tailTS(10, 0, 2), "model-a", "main")
	e := newTailEnv(t, "")
	p := e.path("s1.jsonl")
	tailAppend(t, p, body)
	tl := e.tailer()
	tailScan(t, tl)
	if got := tailCount(t, e.db, "SELECT count(*) FROM replies"); got != 2 {
		t.Fatalf("replies = %d, want 2", got)
	}
	// Replace the file with a new inode: the tailer re-reads it from byte 0.
	if err := os.WriteFile(p+".new", []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p+".new", p); err != nil {
		t.Fatal(err)
	}
	tailScan(t, tl)
	e2 := e.tailer() // a fresh tailer re-reads too
	tailScan(t, e2)
	if got := tailCount(t, e.db, "SELECT count(*) FROM replies"); got != 2 {
		t.Errorf("replies after re-read = %d, want 2", got)
	}
	if got := tailCount(t, e.db, "SELECT count(*) FROM rx_docs WHERE kind = 'reply'"); got != 2 {
		t.Errorf("reply index docs after re-read = %d, want 2", got)
	}
}

func TestRawRepliesSkipObserverAndSubagent(t *testing.T) {
	for _, sub := range []string{"memory-observer-cwd", "subagents"} {
		t.Run(sub, func(t *testing.T) {
			e := newTailEnv(t, sub)
			tailAppend(t, e.path("agent-1.jsonl"), rawAssistant("s-x", "a1", "chatter", tailTS(10, 0, 1), "model-a", "main"))
			tailScan(t, e.tailer())
			if got := tailCount(t, e.db, "SELECT count(*) FROM replies"); got != 0 {
				t.Errorf("replies = %d, want 0", got)
			}
		})
	}
}

func TestRawSessionBranchAndModelLastSeen(t *testing.T) {
	e := newTailEnv(t, "")
	rawScanFile(t, e, "s1.jsonl",
		rawUser("s1", "go", tailTS(10, 0, 0), "feat/one")+
			rawAssistant("s1", "a1", "one", tailTS(10, 0, 1), "model-a", "feat/one")+
			rawUser("s1", "again", tailTS(10, 1, 0), "feat/two")+
			rawAssistant("s1", "a2", "two", tailTS(10, 1, 1), "model-b", "feat/two")+
			rawUser("s1", "no branch", tailTS(10, 2, 0), ""))
	var branch, model string
	if err := e.db.QueryRow(`SELECT git_branch, model FROM sessions WHERE session_id = 's1'`).Scan(&branch, &model); err != nil {
		t.Fatal(err)
	}
	if branch != "feat/two" || model != "model-b" {
		t.Errorf("branch, model = %q, %q; want feat/two, model-b (an empty value keeps the last one)", branch, model)
	}
}

func TestRawSessionModelIgnoresSubagent(t *testing.T) {
	e := newTailEnv(t, "")
	rawScanFile(t, e, "s1.jsonl", rawAssistant("s1", "a1", "main thread", tailTS(10, 0, 1), "model-main", "main"))
	subDir := filepath.Join(e.dir, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tailAppend(t, filepath.Join(subDir, "agent-1.jsonl"), rawTool("s1", "t1", "Read", `{"file_path":"/x"}`, "ok", tailTS(10, 0, 2), "model-sub", "main"))
	tailScan(t, e.tailer())
	var model string
	if err := e.db.QueryRow(`SELECT model FROM sessions WHERE session_id = 's1'`).Scan(&model); err != nil {
		t.Fatal(err)
	}
	if model != "model-main" {
		t.Errorf("model = %q, want model-main (a subagent's model is not the session's)", model)
	}
}

// ---- query parsing ----

func TestParseRawQuery(t *testing.T) {
	tests := []struct {
		name, in string
		want     []rawUnit
	}{
		{"empty", "  ", nil},
		{"words", "Alpha beta", []rawUnit{{"alpha"}, {"beta"}}},
		{"phrase", `"alpha beta" gamma`, []rawUnit{{"alpha", "beta"}, {"gamma"}}},
		{"unterminated quote runs to the end", `x "alpha beta`, []rawUnit{{"x"}, {"alpha", "beta"}}},
		{"punctuation only is dropped", `-- alpha`, []rawUnit{{"alpha"}}},
		{"hyphenated word is a phrase", "foo-bar", []rawUnit{{"foo", "bar"}}},
		{"dotted name stays whole", "parser.go", []rawUnit{{"parser.go"}}},
		{"duplicates collapse", "alpha ALPHA", []rawUnit{{"alpha"}}},
		{"empty quotes", `"" alpha`, []rawUnit{{"alpha"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseRawQuery(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseRawQuery(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// ---- search semantics ----

func TestRawSearchWordsPhrasesAndEmpty(t *testing.T) {
	e := newTailEnv(t, "")
	rawScanFile(t, e, "s1.jsonl", rawUser("s1", "alpha beta delta", tailTS(10, 0, 0), "main")+rawAssistant("s1", "a1", "ok", tailTS(10, 0, 1), "m", "main"))
	rawScanFile(t, e, "s2.jsonl", rawUser("s2", "alpha gamma", tailTS(11, 0, 0), "main")+rawAssistant("s2", "a2", "the alphabet song", tailTS(11, 0, 1), "m", "main"))
	rawScanFile(t, e, "s3.jsonl", rawUser("s3", "something else", tailTS(12, 0, 0), "main")+rawAssistant("s3", "a3", "beta arrives later", tailTS(12, 0, 1), "m", "main"))

	tests := []struct {
		name, q string
		want    []string
	}{
		{"one word", "alpha", []string{"s2", "s1"}},
		{"whole words only", "alph", nil},
		{"every word must match", "alpha beta", []string{"s1"}},
		{"words may sit in different messages of one session", "something beta", []string{"s3"}},
		{"case insensitive", "ALPHA GAMMA", []string{"s2"}},
		{"phrase in order", `"alpha beta"`, []string{"s1"}},
		{"phrase out of order", `"beta alpha"`, nil},
		{"phrase must be adjacent", `"alpha delta"`, nil},
		{"phrase plus word", `"alpha beta" delta`, []string{"s1"}},
		{"unknown word", "zzzunknown", nil},
		{"empty lists newest first", "", []string{"s3", "s2", "s1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rawIDs(mustRaw(t, e, rawQuery{Text: tt.q}))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("rawSearch(%q) = %v, want %v", tt.q, got, tt.want)
			}
		})
	}
}

func TestRawSearchPhraseDoesNotSpanMessages(t *testing.T) {
	e := newTailEnv(t, "")
	rawScanFile(t, e, "s1.jsonl", rawUser("s1", "ends with alpha", tailTS(10, 0, 0), "main")+rawAssistant("s1", "a1", "beta starts here", tailTS(10, 0, 1), "m", "main"))
	if got := mustRaw(t, e, rawQuery{Text: `"alpha beta"`}); len(got) != 0 {
		t.Errorf("phrase across two messages matched: %v", rawIDs(got))
	}
}

func TestRawSearchToolInputNotOutput(t *testing.T) {
	e := newTailEnv(t, "")
	rawScanFile(t, e, "s1.jsonl",
		rawTool("s1", "t1", "Bash", `{"command":"grep -r needleinput src"}`, "found needleoutput here", tailTS(10, 0, 1), "m", "main"))
	if got := rawIDs(mustRaw(t, e, rawQuery{Text: "needleinput"})); !reflect.DeepEqual(got, []string{"s1"}) {
		t.Errorf("tool input not searchable: %v", got)
	}
	if got := mustRaw(t, e, rawQuery{Text: "needleoutput"}); len(got) != 0 {
		t.Errorf("tool output is searchable: %v", rawIDs(got))
	}
}

func TestRawSearchFilePaths(t *testing.T) {
	e := newTailEnv(t, "")
	rawScanFile(t, e, "s1.jsonl",
		rawTool("s1", "t1", "Read", `{"file_path":"/work/demo/internal/parser.go"}`, "package parser", tailTS(10, 0, 1), "m", "main")+
			rawTool("s1", "t2", "NotebookEdit", `{"notebook_path":"/work/demo/analysis.ipynb","new_source":"x"}`, "ok", tailTS(10, 0, 2), "m", "main"))
	for _, q := range []string{"parser.go", "parser", "internal", `"internal parser.go"`, "analysis.ipynb"} {
		if got := rawIDs(mustRaw(t, e, rawQuery{Text: q})); !reflect.DeepEqual(got, []string{"s1"}) {
			t.Errorf("rawSearch(%q) = %v, want [s1]", q, got)
		}
	}
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM rx_docs WHERE kind = 'path'`).Scan(&n); err != nil || n != 2 {
		t.Errorf("path docs = %d (%v), want 2", n, err)
	}
}

func TestRawSearchLongInputStillFindsPath(t *testing.T) {
	e := newTailEnv(t, "")
	big := strings.Repeat("filler ", rawMaxTokens)
	input, _ := json.Marshal(map[string]string{"content": big + " tailmarker", "file_path": "/work/demo/late.go"})
	rawScanFile(t, e, "s1.jsonl", rawTool("s1", "t1", "Write", string(input), "ok", tailTS(10, 0, 1), "m", "main"))
	if got := mustRaw(t, e, rawQuery{Text: "late.go"}); len(got) != 1 {
		t.Errorf("path of a huge Write not found: %v", rawIDs(got))
	}
	if got := mustRaw(t, e, rawQuery{Text: "tailmarker"}); len(got) != 0 {
		t.Errorf("tokens past the cap were indexed: %v", rawIDs(got))
	}
}

func TestRawSearchFilters(t *testing.T) {
	e := newTailEnv(t, "")
	rawScanFile(t, e, "s1.jsonl", rawUser("s1", "common thing", tailTS(10, 0, 0), "feat/a")+rawAssistant("s1", "a1", "ok", tailTS(10, 0, 1), "claude-opus-5-5", "feat/a"))
	rawScanFile(t, e, "s2.jsonl", rawUser("s2", "common thing", tailTS(11, 0, 0), "main")+rawAssistant("s2", "a2", "ok", tailTS(11, 0, 1), "claude-haiku-5-5", "main"))
	rawScanFile(t, e, "s3.jsonl", rawUser("s3", "common thing", tailTS(12, 0, 0), "main")+rawAssistant("s3", "a3", "ok", tailTS(12, 0, 1), "claude-opus-5-5", "main"))
	// An observer session never shows up.
	obsDir := filepath.Join(e.dir, "memory-observer-cwd")
	if err := os.MkdirAll(obsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tailAppend(t, filepath.Join(obsDir, "o.jsonl"), rawUser("s4", "common thing", tailTS(13, 0, 0), "main"))
	tailScan(t, e.tailer())

	start := time.Date(2026, 10, 28, 10, 30, 0, 0, time.UTC)
	end := time.Date(2026, 10, 28, 11, 30, 0, 0, time.UTC)
	tests := []struct {
		name string
		q    rawQuery
		want []string
	}{
		{"no filter, observer excluded", rawQuery{Text: "common"}, []string{"s3", "s2", "s1"}},
		{"branch", rawQuery{Text: "common", Branch: "main"}, []string{"s3", "s2"}},
		{"branch is exact", rawQuery{Text: "common", Branch: "mai"}, nil},
		{"model substring", rawQuery{Text: "common", Model: "opus"}, []string{"s3", "s1"}},
		{"branch and model", rawQuery{Text: "common", Branch: "main", Model: "opus"}, []string{"s3"}},
		{"date range", rawQuery{Text: "common", Start: &start, End: &end}, []string{"s2"}},
		{"empty query keeps filters", rawQuery{Branch: "feat/a"}, []string{"s1"}},
		{"limit and offset", rawQuery{Text: "common", Limit: 1, Offset: 1}, []string{"s2"}},
		{"project", rawQuery{Text: "common", Project: projectKey("/work/demo")}, []string{"s3", "s2", "s1"}},
		{"other project", rawQuery{Text: "common", Project: "elsewhere"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := tt.q
			if q.Project != "" {
				q.Keys = projectKeys(e.db, q.Project)
			}
			got := rawIDs(mustRaw(t, e, q))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRawSearchSnippet(t *testing.T) {
	e := newTailEnv(t, "")
	long := strings.Repeat("lorem ipsum ", 40) + "the needle word" + strings.Repeat(" dolor sit", 40)
	rawScanFile(t, e, "s1.jsonl", rawUser("s1", "start", tailTS(10, 0, 0), "main")+rawAssistant("s1", "a1", long, tailTS(10, 0, 1), "m", "main"))
	hits := mustRaw(t, e, rawQuery{Text: "needle"})
	if len(hits) != 1 {
		t.Fatalf("hits = %v", rawIDs(hits))
	}
	sn := hits[0].Snippet
	if !strings.Contains(sn, "needle") || !strings.HasPrefix(sn, "[reply]") {
		t.Errorf("snippet = %q, want a [reply] snippet around the match", sn)
	}
	if len(sn) > 260 {
		t.Errorf("snippet is %d bytes, want it short", len(sn))
	}
	// An empty query shows the first prompt as the session title.
	hits = mustRaw(t, e, rawQuery{})
	if len(hits) != 1 || !strings.Contains(hits[0].Snippet, "start") {
		t.Errorf("empty-query snippet = %+v", hits)
	}
}

// ---- incremental index, catch-up, backfill ----

func TestRawIndexIncrementalNoDuplicates(t *testing.T) {
	e := newTailEnv(t, "")
	p := e.path("s1.jsonl")
	tl := e.tailer()
	tailAppend(t, p, rawUser("s1", "first topic", tailTS(10, 0, 0), "main"))
	tailScan(t, tl)
	if got := rawIDs(mustRaw(t, e, rawQuery{Text: "later"})); len(got) != 0 {
		t.Fatalf("later found early: %v", got)
	}
	tailAppend(t, p, rawAssistant("s1", "a1", "later topic", tailTS(10, 0, 1), "m", "main"))
	tailScan(t, tl)
	if got := rawIDs(mustRaw(t, e, rawQuery{Text: "later"})); !reflect.DeepEqual(got, []string{"s1"}) {
		t.Errorf("new reply not searchable: %v", got)
	}
	before := tailCount(t, e.db, "SELECT count(*) FROM rx_post")
	for i := 0; i < 3; i++ {
		if err := memdRawIndex(e.db); err != nil {
			t.Fatal(err)
		}
	}
	if after := tailCount(t, e.db, "SELECT count(*) FROM rx_post"); after != before {
		t.Errorf("rx_post grew on re-index: %d -> %d", before, after)
	}
}

func TestRawIndexBackfillsExistingRows(t *testing.T) {
	db, _ := openTestMemd(t)
	for _, s := range []string{
		`INSERT INTO sessions (session_id, project, cwd) VALUES ('old', 'p', '/w')`,
		`INSERT INTO prompts (session_id, prompt_number, text, created_at) VALUES ('old', 1, 'legacyprompt words', TIMESTAMP '2026-01-01 10:00:00')`,
		`INSERT INTO tool_uses (session_id, tool_use_id, tool_name, input, output, created_at) VALUES ('old', 't1', 'Read', '{"file_path":"/w/legacy.go"}', 'out', TIMESTAMP '2026-01-01 10:00:01')`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	// A search catches up rows that landed behind the write hook.
	hits, err := rawSearch(db, rawQuery{Text: "legacyprompt legacy.go"})
	if err != nil || !reflect.DeepEqual(rawIDs(hits), []string{"old"}) {
		t.Errorf("hits = %v, err = %v", rawIDs(hits), err)
	}
	// Re-opening the database (migration) is a no-op for rows already indexed.
	n := tailCount(t, db, "SELECT count(*) FROM rx_post")
	if err := memdMigrate(db); err != nil {
		t.Fatal(err)
	}
	if got := tailCount(t, db, "SELECT count(*) FROM rx_post"); got != n {
		t.Errorf("rx_post changed on migrate: %d -> %d", n, got)
	}
}

// ---- tools ----

func TestRawToolsRegistered(t *testing.T) {
	if len(mcpTools) != 12 {
		t.Errorf("mcpTools = %d, want 12", len(mcpTools))
	}
	if !mcpKnownTool("read_session") {
		t.Error("read_session is not an MCP tool")
	}
	if memdTools["read_session"] == nil {
		t.Error("read_session is not served over /tool/{name}")
	}
}

func TestMemSearchFallsBackToRaw(t *testing.T) {
	e := newTailEnv(t, "")
	rawScanFile(t, e, "s1.jsonl", rawUser("s1", "investigate the zorblax glitch", tailTS(10, 0, 0), "feat/z")+rawAssistant("s1", "a1", "done", tailTS(10, 0, 1), "claude-opus-5-5", "feat/z"))

	out, err := memSearch(e.db, map[string]any{"query": "zorblax"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "s1") || !strings.Contains(out, "zorblax") || !strings.Contains(out, "feat/z") {
		t.Errorf("fallback output = %q, want the raw session hit", out)
	}
	out, _ = memSearch(e.db, map[string]any{"query": "zorblax", "source": "notes"})
	if !strings.HasPrefix(out, "No results found") {
		t.Errorf("source=notes output = %q, want no results", out)
	}
	out, _ = memSearch(e.db, map[string]any{"query": "zorblax", "source": "raw", "branch": "feat/other"})
	if !strings.HasPrefix(out, "No results found") {
		t.Errorf("branch filter output = %q, want no results", out)
	}
	out, _ = memSearch(e.db, map[string]any{"query": "zorblax", "branch": "feat/z", "model": "opus"})
	if !strings.Contains(out, "s1") {
		t.Errorf("branch+model output = %q, want s1 (branch implies raw)", out)
	}
}

func TestMemSearchRawOnlyWhenNotesMiss(t *testing.T) {
	e := newTailEnv(t, "")
	rawScanFile(t, e, "s1.jsonl", rawUser("s1", "widget talk", tailTS(10, 0, 0), "main"))
	if _, err := e.db.Exec(`INSERT INTO observations (project, session_id, type, title, narrative, source) VALUES ('p', 's9', 'discovery', 'widget note', 'about widget', 'live')`); err != nil {
		t.Fatal(err)
	}
	out, err := memSearch(e.db, map[string]any{"query": "widget"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "widget note") || strings.Contains(out, "raw session") {
		t.Errorf("notes hit should win without raw fallback: %q", out)
	}
}

func TestReadSession(t *testing.T) {
	e := newTailEnv(t, "")
	bigOut := strings.Repeat("x", 3000)
	rawScanFile(t, e, "s1.jsonl",
		rawUser("s1", "please fix it", tailTS(10, 0, 0), "feat/q")+
			rawAssistant("s1", "a1", "looking now", tailTS(10, 0, 1), "claude-opus-5-5", "feat/q")+
			rawTool("s1", "t1", "Bash", `{"command":"go test ./..."}`, bigOut, tailTS(10, 0, 2), "claude-opus-5-5", "feat/q")+
			rawAssistant("s1", "a2", "all green", tailTS(10, 0, 3), "claude-opus-5-5", "feat/q"))
	out, err := memReadSession(e.db, map[string]any{"session_id": "s1"})
	if err != nil {
		t.Fatal(err)
	}
	order := []string{"please fix it", "looking now", "go test ./...", "all green"}
	last := -1
	for _, want := range order {
		i := strings.Index(out, want)
		if i < 0 || i < last {
			t.Fatalf("%q missing or out of order in:\n%s", want, out)
		}
		last = i
	}
	if strings.Contains(out, bigOut) {
		t.Error("tool output was not truncated")
	}
	if !strings.Contains(out, "feat/q") || !strings.Contains(out, "claude-opus-5-5") {
		t.Errorf("header lacks branch or model:\n%s", out)
	}
	page, err := memReadSession(e.db, map[string]any{"session_id": "s1", "offset": 1, "limit": 2})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page, "please fix it") || !strings.Contains(page, "looking now") || !strings.Contains(page, "go test") || strings.Contains(page, "all green") {
		t.Errorf("offset/limit page wrong:\n%s", page)
	}
	if !strings.Contains(page, "offset=3") {
		t.Errorf("page does not say how to continue:\n%s", page)
	}
	if _, err := memReadSession(e.db, map[string]any{"session_id": "nope"}); err == nil {
		t.Error("unknown session: want an error")
	}
	if _, err := memReadSession(e.db, map[string]any{}); err == nil {
		t.Error("missing session_id: want an error")
	}
}

func TestReadSessionMasksStoredText(t *testing.T) {
	e := newTailEnv(t, "")
	rawScanFile(t, e, "s1.jsonl", rawAssistant("s1", "a1", "token "+memdFakeGH+" ok", tailTS(10, 0, 1), "m", "main"))
	out, err := memReadSession(e.db, map[string]any{"session_id": "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, memdFakeGH) {
		t.Errorf("secret in read_session output: %s", out)
	}
	if got := tailCount(t, e.db, "SELECT count(*) FROM replies WHERE text LIKE '%'||?||'%'", memdFakeGH); got != 0 {
		t.Errorf("secret stored in replies")
	}
}
