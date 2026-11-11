package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// aliasRepo makes a plain (non-git) directory called name under parent and returns it with its
// new key and legacy key. Non-git, so the new key is "<name>-<hash>" and the legacy key "<name>".
func aliasRepo(t *testing.T, parent, name string) (dir, key, legacy string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "cfg"))
	dir = filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, projectKey(dir), legacyProjectKey(dir)
}

// seedLegacy writes one session plus an observation, summary, prompt, tool use, file row,
// a fact and a handoff under the old key, tagged with tag so reads can be told apart.
func seedLegacy(t *testing.T, db *sql.DB, legacy, cwd, tag string) (obsID, sumID int64) {
	t.Helper()
	sid := "sess-" + tag
	ctxExec(t, db, `INSERT INTO sessions (session_id, project, cwd) VALUES (?, ?, ?)`, sid, legacy, cwd)
	if err := db.QueryRow(`INSERT INTO observations (project, session_id, type, raw_type, title, narrative, source)
		VALUES (?, ?, 'discovery', 'discovery', ?, ?, 'live') RETURNING id`, legacy, sid, "oldnote"+tag, "narr"+tag).Scan(&obsID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`INSERT INTO summaries (project, session_id, request, source)
		VALUES (?, ?, ?, 'live') RETURNING id`, legacy, sid, "oldsummary"+tag).Scan(&sumID); err != nil {
		t.Fatal(err)
	}
	ctxExec(t, db, `INSERT INTO prompts (session_id, prompt_number, text) VALUES (?, 1, ?)`, sid, "oldprompt"+tag)
	ctxExec(t, db, `INSERT INTO tool_uses (session_id, tool_use_id, tool_name, input, output) VALUES (?, ?, 'Bash', 'in', 'out')`, sid, "tu-"+tag)
	ctxExec(t, db, `INSERT INTO observation_files (observation_id, path, kind) VALUES (?, ?, 'read')`, obsID, "src/old"+tag+".go")
	b, _ := json.Marshal(map[string]any{"fact" + tag: "oldfact" + tag})
	ctxExec(t, db, `INSERT INTO work_state (project, list_name, fields) VALUES (?, 'facts', ?)`, legacy, string(b))
	seedHandoff(t, db, legacy, hoEntry("oldhandoff"+tag), hoNow.Add(-time.Hour))
	return obsID, sumID
}

func aliasRows(t *testing.T, db *sql.DB) [][2]string {
	t.Helper()
	rows, err := db.Query(`SELECT old_key, new_key FROM project_aliases ORDER BY 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var r [2]string
		if err := rows.Scan(&r[0], &r[1]); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// tableDump renders every row of the tables a project key lives in, for byte-for-byte checks.
func tableDump(t *testing.T, db *sql.DB) string {
	t.Helper()
	var b strings.Builder
	for _, tbl := range []string{"sessions", "observations", "summaries", "work_state", "prompts", "tool_uses", "observation_files"} {
		rows, err := db.Query(`SELECT CAST(x AS VARCHAR) FROM ` + tbl + ` x ORDER BY CAST(x AS VARCHAR)`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			b.WriteString(tbl + "|" + s + "\n")
		}
		rows.Close()
	}
	return b.String()
}

func TestEnsureProjectAliasCreation(t *testing.T) {
	db, _ := openTestMemd(t)
	root := t.TempDir()
	dir, key, legacy := aliasRepo(t, root, "alpha")

	ensureProjectAlias(db, dir)
	if got := aliasRows(t, db); len(got) != 0 {
		t.Fatalf("alias created with no old rows: %v", got)
	}

	// the daemon memoizes "nothing to alias", so a fresh DB handle sees rows added later
	db2, _ := openTestMemd(t)
	seedLegacy(t, db2, legacy, dir, "x")
	ensureProjectAlias(db2, dir)
	ensureProjectAlias(db2, dir) // idempotent
	if got, want := aliasRows(t, db2), [][2]string{{legacy, key}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("aliases = %v, want %v", got, want)
	}

	// blank cwd and unknown-project never alias
	ensureProjectAlias(db2, "")
	ensureProjectAlias(db2, "/")
	if got := aliasRows(t, db2); len(got) != 1 {
		t.Fatalf("aliases after blank cwd = %v", got)
	}
}

func TestProjectKeysFollowsAliases(t *testing.T) {
	db, _ := openTestMemd(t)
	ctxExec(t, db, `INSERT INTO project_aliases (old_key, new_key) VALUES ('b-old', 'N'), ('a-old', 'N'), ('a-old', 'M')`)
	if got, want := projectKeys(db, "N"), []string{"N", "a-old", "b-old"}; !reflect.DeepEqual(got, want) {
		t.Errorf("projectKeys(N) = %v, want %v", got, want)
	}
	if got, want := projectKeys(db, "a-old"), []string{"a-old"}; !reflect.DeepEqual(got, want) {
		t.Errorf("an old key does not expand: %v", got)
	}
	if got, want := projectKeys(db, "none"), []string{"none"}; !reflect.DeepEqual(got, want) {
		t.Errorf("projectKeys(none) = %v", got)
	}
	// Dropping the table is the documented undo: reads fall back to the raw key.
	ctxExec(t, db, `DROP TABLE project_aliases`)
	if got, want := projectKeys(db, "N"), []string{"N"}; !reflect.DeepEqual(got, want) {
		t.Errorf("projectKeys after drop = %v, want %v", got, want)
	}
}

func TestAliasReadPathsFollow(t *testing.T) {
	db, _ := openTestMemd(t)
	root := t.TempDir()
	dir, key, legacy := aliasRepo(t, root, "beta")
	obsID, sumID := seedLegacy(t, db, legacy, dir, "beta")
	// A different project's rows must stay out of every read.
	seedLegacy(t, db, "other", filepath.Join(root, "elsewhere"), "other")
	ensureProjectAlias(db, dir)

	// A row written under the new key sits next to the old ones.
	ctxExec(t, db, `INSERT INTO sessions (session_id, project, cwd) VALUES ('sess-new', ?, ?)`, key, dir)
	ctxExec(t, db, `INSERT INTO observations (project, session_id, type, raw_type, title, narrative, source)
		VALUES (?, 'sess-new', 'discovery', 'discovery', 'newnote', 'n', 'live')`, key)

	has := func(name, got string, want ...string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: missing %q in:\n%s", name, w, got)
			}
		}
		if strings.Contains(got, "oldnoteother") || strings.Contains(got, "oldsummaryother") || strings.Contains(got, "oldfactother") {
			t.Errorf("%s: leaked another project's rows:\n%s", name, got)
		}
	}
	idStr := func(id int64) any { return float64(id) }

	has("buildContext", buildContext(db, key, hoNow.Add(2*time.Hour)), "oldnotebeta", "newnote", "oldfactbeta")

	out, err := memSearch(db, map[string]any{"query": "oldnotebeta", "project": key})
	if err != nil {
		t.Fatal(err)
	}
	has("search observations (fts)", out, "oldnotebeta")
	out, _ = memSearch(db, map[string]any{"query": "oldsummarybeta", "project": key})
	has("search summaries", out, "oldsummarybeta")
	out, _ = memSearch(db, map[string]any{"query": "oldpromptbeta", "project": key})
	has("search prompts", out, "oldpromptbeta")
	out, _ = memSearch(db, map[string]any{"query": "src/oldbeta.go", "project": key})
	has("search observations (path)", out, "oldnotebeta")

	// The in-memory index filters on its own interned ids.
	searcherFor(db).buildAll(context.Background())
	out, _ = memSearch(db, map[string]any{"query": "oldnotebeta", "project": key})
	has("search observations (in-memory index)", out, "oldnotebeta")
	out, _ = memSearch(db, map[string]any{"query": "oldsummarybeta", "project": key})
	has("search summaries (in-memory index)", out, "oldsummarybeta")

	out, _ = memTimeline(db, map[string]any{"anchor": idStr(obsID), "project": key})
	has("timeline anchor", out, "oldnotebeta", "newnote")
	out, _ = memGetObservations(db, map[string]any{"ids": []any{idStr(obsID), "S" + itoa(sumID)}, "project": key})
	has("get_observations", out, "oldnotebeta", "oldsummarybeta")
	out, _ = memGetToolUses(db, map[string]any{"ids": []any{"tu-beta"}, "project": key})
	has("get_tool_uses", out, "Bash")

	body, err := byFile(db, "src/oldbeta.go", key)
	if err != nil {
		t.Fatal(err)
	}
	has("by-file", body, "oldnotebeta")

	lists, err := workStateFoldRows(db, key)
	if err != nil {
		t.Fatal(err)
	}
	if l := lists[wsFactsList]; l == nil {
		t.Errorf("workStateFoldRows: no facts list via alias: %v", lists)
	}
	e, err := takeHandoff(db, key, "startup", hoNow)
	if err != nil || e == nil || fmt.Sprint(e.fields["text"]) != "oldhandoffbeta" {
		t.Errorf("takeHandoff via alias = %+v, %v", e, err)
	}

	h := uiHandler(db)
	for name, target := range map[string]string{
		"api observations": "/api/v1/observations?project=" + key,
		"api sessions":     "/api/v1/sessions?project=" + key,
		"api handoffs":     "/api/v1/handoffs?project=" + key,
		"api work-state":   "/api/v1/work-state?project=" + key,
	} {
		code, body := apiGet(t, h, target)
		if code != 200 {
			t.Errorf("%s = %d: %s", name, code, body)
			continue
		}
		switch name {
		case "api observations":
			has(name, body, "oldnotebeta", "newnote")
		case "api sessions":
			has(name, body, "sess-beta", "sess-new")
		case "api handoffs":
			has(name, body, "oldhandoffbeta")
		case "api work-state":
			has(name, body, "oldfactbeta")
		}
	}
	code, body := apiGet(t, h, "/api/v1/timeseries?project="+key)
	if code != 200 || !strings.Contains(body, key) && !strings.Contains(body, legacy) {
		t.Errorf("timeseries = %d: %s", code, body)
	}

	// Without the alias the old rows are not found: the alias is what does the work.
	ctxExec(t, db, `DROP TABLE project_aliases`)
	out, _ = memSearch(db, map[string]any{"query": "oldnotebeta", "project": key})
	if !strings.Contains(out, "No results") {
		t.Errorf("old note still visible after dropping the alias table:\n%s", out)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestAliasSharedBasename(t *testing.T) {
	db, _ := openTestMemd(t)
	root := t.TempDir()
	a, keyA, legacy := aliasRepo(t, filepath.Join(root, "work"), "ui")
	b, keyB, legacyB := aliasRepo(t, filepath.Join(root, "play"), "ui")
	if legacy != legacyB || keyA == keyB {
		t.Fatalf("setup: legacy %q/%q keys %q/%q", legacy, legacyB, keyA, keyB)
	}

	// Only repo A has sessions under "ui": A gets the alias, B (a different directory) does not.
	seedLegacy(t, db, legacy, a, "a")
	ensureProjectAlias(db, a)
	ensureProjectAlias(db, b)
	if got, want := aliasRows(t, db), [][2]string{{legacy, keyA}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("aliases = %v, want %v", got, want)
	}
	if got := buildContext(db, keyB, hoNow); strings.Contains(got, "oldnotea") {
		t.Errorf("repo B sees repo A's note:\n%s", got)
	}
	if got := buildContext(db, keyA, hoNow); !strings.Contains(got, "oldnotea") {
		t.Errorf("repo A lost its own note:\n%s", got)
	}

	// Both repos have sessions under "ui": they cannot be split row by row, so neither gets an alias.
	db2, _ := openTestMemd(t)
	seedLegacy(t, db2, legacy, a, "a")
	seedLegacy(t, db2, legacy, b, "b")
	ensureProjectAlias(db2, a)
	ensureProjectAlias(db2, b)
	if got := aliasRows(t, db2); len(got) != 0 {
		t.Errorf("ambiguous shared key was aliased: %v", got)
	}
	if got := buildContext(db2, keyA, hoNow); strings.Contains(got, "oldnoteb") || strings.Contains(got, "oldnotea") {
		t.Errorf("ambiguous shared key leaked into repo A:\n%s", got)
	}

	// No cwd evidence at all (manual notes, imports): cannot be told apart, so every claimant aliases it.
	db3, _ := openTestMemd(t)
	seedLegacy(t, db3, legacy, "", "m")
	ensureProjectAlias(db3, a)
	ensureProjectAlias(db3, b)
	wantRows := [][2]string{{legacy, keyA}, {legacy, keyB}}
	if keyB < keyA {
		wantRows[0], wantRows[1] = wantRows[1], wantRows[0]
	}
	if got, want := aliasRows(t, db3), wantRows; !reflect.DeepEqual(got, want) {
		t.Errorf("aliases = %v, want %v", got, want)
	}
	for _, k := range []string{keyA, keyB} {
		if got := buildContext(db3, k, hoNow); !strings.Contains(got, "oldnotem") {
			t.Errorf("%s did not see the unattributable note:\n%s", k, got)
		}
	}
}

func TestAliasLeavesOldRowsUntouched(t *testing.T) {
	db, _ := openTestMemd(t)
	root := t.TempDir()
	dir, key, legacy := aliasRepo(t, root, "gamma")
	seedLegacy(t, db, legacy, dir, "g")
	before := tableDump(t, db)

	ensureProjectAlias(db, dir)
	buildContext(db, key, hoNow)
	_, _ = memSearch(db, map[string]any{"query": "oldnoteg", "project": key})
	_, _ = takeHandoffNoConsume(db, key)
	if after := tableDump(t, db); after != before {
		t.Errorf("existing rows changed:\n--- before\n%s--- after\n%s", before, after)
	}

	// New writes use the new key and append; the old rows are still byte-for-byte there.
	if _, err := memWorkStateWrite(db, map[string]any{"list": "facts", "fields": map[string]any{"k": "v"}, "cwd": dir}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM work_state WHERE project = ? AND list_name = 'facts'`, key).Scan(&n); err != nil || n != 1 {
		t.Errorf("new work_state rows under new key = %d, %v", n, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM work_state WHERE project = ?`, legacy).Scan(&n); err != nil || n != 2 {
		t.Errorf("old key work_state rows = %d, %v (want the 2 seeded)", n, err)
	}
	if _, err := memSaveMemory(db, map[string]any{"text": "fresh note", "title": "fresh", "cwd": dir}); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM observations WHERE project = ? AND title = 'fresh'`, key).Scan(&n); err != nil || n != 1 {
		t.Errorf("save_memory under new key = %d, %v", n, err)
	}
}

// takeHandoffNoConsume reads the project's handoff list without claiming it.
func takeHandoffNoConsume(db *sql.DB, project string) (int, error) {
	lists, err := workStateFoldRows(db, project)
	return len(lists), err
}

func TestContextEndpointCreatesAlias(t *testing.T) {
	db, _ := openTestMemd(t)
	root := t.TempDir()
	dir, key, legacy := aliasRepo(t, root, "delta")
	seedLegacy(t, db, legacy, dir, "d")

	h := memdHandler(db)
	code, body := apiGet(t, h, "/context?project="+key+"&cwd="+dir)
	if code != 200 || !strings.Contains(body, "oldnoted") {
		t.Fatalf("/context with cwd = %d, old note missing:\n%s", code, body)
	}
	if got, want := aliasRows(t, db), [][2]string{{legacy, key}}; !reflect.DeepEqual(got, want) {
		t.Errorf("aliases = %v, want %v", got, want)
	}
}

func TestMigrateAddsAliasTableAdditively(t *testing.T) {
	db, dir := openTestMemd(t)
	if !contains(memdTables(t, db), "project_aliases") {
		t.Fatalf("project_aliases missing: %v", memdTables(t, db))
	}
	ctxExec(t, db, `DROP TABLE project_aliases`)
	db.Close()
	db2, err := openMemdDB(dir) // an upgrade over a database that lacks the table
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if !contains(memdTables(t, db2), "project_aliases") {
		t.Errorf("migration did not recreate project_aliases")
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
