package main

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

// Fixtures are synthetic. Expected strings are the tools' fixed reply texts.

func TestMemSaveMemory(t *testing.T) {
	db, _ := openTestMemd(t)

	out, err := memSaveMemory(db, map[string]any{"text": "retry budget is three attempts for the flaky widget sync", "title": "Widget sync retries", "project": "proj-a"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "Saved memory #1: Widget sync retries. Project: proj-a." {
		t.Errorf("reply = %q", out)
	}
	var typ, sub, src string
	if err := db.QueryRow(`SELECT type, subtitle, source FROM observations WHERE id = 1`).Scan(&typ, &sub, &src); err != nil {
		t.Fatal(err)
	}
	if typ != "discovery" || sub != "Manual memory" || src != "live" {
		t.Errorf("row = %q %q %q", typ, sub, src)
	}

	found, err := memSearch(db, map[string]any{"query": "widget", "project": "proj-a"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(found, "Widget sync retries") || !strings.Contains(found, "#1") {
		t.Errorf("search did not find the saved memory:\n%s", found)
	}
	other, _ := memSearch(db, map[string]any{"query": "widget", "project": "proj-b"})
	if strings.Contains(other, "Widget sync retries") {
		t.Errorf("project scoping leaked:\n%s", other)
	}
}

func TestMemSaveMemoryDefaultsAndType(t *testing.T) {
	db, _ := openTestMemd(t)
	long := strings.Repeat("alpha ", 20)
	out, err := memSaveMemory(db, map[string]any{"text": "  " + long + "  ", "project": "p", "type": "decision"})
	if err != nil {
		t.Fatal(err)
	}
	var title, typ string
	db.QueryRow(`SELECT title, type FROM observations WHERE id = 1`).Scan(&title, &typ)
	want := strings.TrimSpace(strings.TrimSpace(long)[:60]) + "..."
	if title != want || typ != "decision" {
		t.Errorf("title %q type %q, want %q decision", title, typ, want)
	}
	if !strings.HasPrefix(out, "Saved memory #1: "+want+".") {
		t.Errorf("reply = %q", out)
	}
	if _, err := memSaveMemory(db, map[string]any{"text": "x", "project": "p", "type": "nonsense"}); err != nil {
		t.Fatal(err)
	}
	db.QueryRow(`SELECT type FROM observations WHERE id = 2`).Scan(&typ)
	if typ != "discovery" {
		t.Errorf("unknown type stored as %q, want discovery", typ)
	}
}

func TestMemSaveMemoryEmptyText(t *testing.T) {
	db, _ := openTestMemd(t)
	for _, text := range []any{nil, "", "   ", 5} {
		out, err := memSaveMemory(db, map[string]any{"text": text})
		if err != nil || !strings.HasPrefix(out, "Error: ") {
			t.Errorf("text %v: out %q err %v, want an Error: reply", text, out, err)
		}
	}
}

func TestMemSaveMemoryRedacts(t *testing.T) {
	db, _ := openTestMemd(t)
	secret := "sk-ant-api03-" + strings.Repeat("Ab3dEf9h", 6)
	if _, err := memSaveMemory(db, map[string]any{"text": "deploy uses key " + secret + " today", "title": "key " + secret, "project": "p"}); err != nil {
		t.Fatal(err)
	}
	var title, narr string
	db.QueryRow(`SELECT title, narrative FROM observations WHERE id = 1`).Scan(&title, &narr)
	if strings.Contains(title+narr, secret) || strings.Contains(title+narr, "Ab3dEf9h") {
		t.Errorf("secret stored: %q %q", title, narr)
	}
	if !strings.Contains(narr, "deploy uses key") {
		t.Errorf("surrounding text lost: %q", narr)
	}
}

func TestMemSaveMemoryDefaultProject(t *testing.T) {
	db, _ := openTestMemd(t)
	dir := t.TempDir()
	if _, err := memSaveMemory(db, map[string]any{"text": "note", "cwd": dir}); err != nil {
		t.Fatal(err)
	}
	var p string
	db.QueryRow(`SELECT project FROM observations WHERE id = 1`).Scan(&p)
	if p != projectKey(dir) {
		t.Errorf("project = %q, want %q", p, projectKey(dir))
	}
}

// memory runs in its own directory, so a call without project or cwd must fail rather than
// file the note under the daemon's directory.
func TestMemdToolsRequireClientCheckout(t *testing.T) {
	db, _ := openTestMemd(t)
	calls := map[string]func() (string, error){
		"save_memory": func() (string, error) { return memSaveMemory(db, map[string]any{"text": "note"}) },
		"work_state_write": func() (string, error) {
			return memWorkStateWrite(db, map[string]any{"list": "l", "fields": map[string]any{"k": "v"}})
		},
		"work_state_read": func() (string, error) { return memWorkStateRead(db, map[string]any{}) },
	}
	for name, call := range calls {
		out, err := call()
		if err != nil || !strings.HasPrefix(out, "Error:") {
			t.Errorf("%s without cwd = %q, %v; want an Error: reply", name, out, err)
		}
	}
	for q, want := range map[string]int{"SELECT count(*) FROM observations": 0, "SELECT count(*) FROM work_state": 0} {
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil || n != want {
			t.Errorf("%s = %d (%v), want %d", q, n, err, want)
		}
	}
}

func wsWrite(t *testing.T, db *sql.DB, cwd, list string, fields map[string]any) string {
	t.Helper()
	out, err := memWorkStateWrite(db, map[string]any{"cwd": cwd, "list": list, "fields": fields})
	if err != nil {
		t.Fatalf("write %s: %v", list, err)
	}
	return out
}

func TestMemWorkStateRoundTrip(t *testing.T) {
	db, _ := openTestMemd(t)
	dir := t.TempDir()
	proj := projectKey(dir)

	out := wsWrite(t, db, dir, "release", map[string]any{"task": "tag build", "status": "todo", "owner": "sam"})
	want := "Entry added to list \"release\" (" + proj + "). Still open:\n" +
		"- release\n" +
		"  - [todo] tag build (owner=sam), updated 1 minute ago"
	if out != want {
		t.Errorf("write reply:\n%q\nwant\n%q", out, want)
	}

	wsWrite(t, db, dir, "release", map[string]any{"goal": "ship v2", "n": 3, "ok": true})
	wsWrite(t, db, dir, "release", map[string]any{"task": "announce", "status": "doing"})
	wsWrite(t, db, dir, "release", map[string]any{"task": "tag build", "status": "done"})

	now := time.Now().Add(3 * time.Hour)
	got, err := memWorkStateReadAt(db, map[string]any{"cwd": dir}, now)
	if err != nil {
		t.Fatal(err)
	}
	want = "- release: goal=ship v2, n=3, ok=true, updated about 3 hours ago\n" +
		"  - [doing] announce, updated about 3 hours ago"
	if got != want {
		t.Errorf("read:\n%q\nwant\n%q", got, want)
	}

	got, _ = memWorkStateReadAt(db, map[string]any{"cwd": dir, "includeClosed": true}, now)
	want = "- release: goal=ship v2, n=3, ok=true, updated about 3 hours ago\n" +
		"  - [done] tag build (owner=sam), updated about 3 hours ago\n" +
		"  - [doing] announce, updated about 3 hours ago"
	if got != want {
		t.Errorf("read includeClosed:\n%q\nwant\n%q", got, want)
	}

	// closing the list hides it
	wsWrite(t, db, dir, "release", map[string]any{"status": "done"})
	got, _ = memWorkStateReadAt(db, map[string]any{"cwd": dir}, now)
	if got != "No open items for "+proj+"; set includeClosed to include finished ones." {
		t.Errorf("after close: %q", got)
	}
	got, _ = memWorkStateReadAt(db, map[string]any{"cwd": dir, "list": "other"}, now)
	if got != `No open items in "other" for `+proj+`; set includeClosed to include finished ones.` {
		t.Errorf("empty list read: %q", got)
	}
	got, _ = memWorkStateReadAt(db, map[string]any{"cwd": t.TempDir(), "includeClosed": true}, now)
	if !strings.HasPrefix(got, "No work state recorded for ") {
		t.Errorf("nothing recorded: %q", got)
	}
}

func TestMemWorkStateWriteClosesLastItem(t *testing.T) {
	db, _ := openTestMemd(t)
	dir := t.TempDir()
	wsWrite(t, db, dir, "x", map[string]any{"task": "a", "status": "todo"})
	out := wsWrite(t, db, dir, "x", map[string]any{"task": "a", "status": "done"})
	want := "Entry added to list \"x\" (" + projectKey(dir) + "). No items are left open."
	if out != want {
		t.Errorf("reply = %q, want %q", out, want)
	}
}

func TestMemWorkStateFactsAndUserFacts(t *testing.T) {
	db, _ := openTestMemd(t)
	d1, d2 := t.TempDir(), t.TempDir()
	out := wsWrite(t, db, d1, "Facts", map[string]any{"db": "duckdb"}) // reserved names are matched case-insensitively and stored lowercase
	if !strings.HasPrefix(out, "Entry added to list \"facts\" ("+projectKey(d1)+"). Still open:\n- facts: db=duckdb, updated ") {
		t.Errorf("facts reply = %q", out)
	}
	out = wsWrite(t, db, d1, "user-facts", map[string]any{"editor": "vim"})
	if !strings.HasPrefix(out, `Entry added to list "user-facts" (all projects). Still open:`) {
		t.Errorf("user-facts reply = %q", out)
	}
	// R33: a task key in a durable list is an ordinary fact key
	wsWrite(t, db, d1, "facts", map[string]any{"task": "t", "status": "done"})
	got, _ := memWorkStateRead(db, map[string]any{"cwd": d2})
	if !strings.Contains(got, "- user-facts: editor=vim") || strings.Contains(got, "duckdb") {
		t.Errorf("other project read:\n%s", got)
	}
	got, _ = memWorkStateRead(db, map[string]any{"cwd": d1})
	if !strings.Contains(got, "- facts: db=duckdb, status=done, task=t,") {
		t.Errorf("facts read:\n%s", got)
	}
	if !strings.Contains(got, "- user-facts: editor=vim") {
		t.Errorf("user-facts missing:\n%s", got)
	}
}

func TestMemWorkStateRedactsAndValidates(t *testing.T) {
	db, _ := openTestMemd(t)
	dir := t.TempDir()
	secret := "sk-ant-api03-" + strings.Repeat("Ab3dEf9h", 6)
	wsWrite(t, db, dir, "notes", map[string]any{"note": "token " + secret})
	var raw string
	db.QueryRow(`SELECT fields FROM work_state`).Scan(&raw)
	if strings.Contains(raw, "Ab3dEf9h") {
		t.Errorf("secret stored: %s", raw)
	}

	bad := []struct {
		name string
		args map[string]any
	}{
		{"no list", map[string]any{"fields": map[string]any{"a": "b"}}},
		{"blank list", map[string]any{"list": "  ", "fields": map[string]any{"a": "b"}}},
		{"long list", map[string]any{"list": strings.Repeat("x", 201), "fields": map[string]any{"a": "b"}}},
		{"no fields", map[string]any{"list": "l"}},
		{"empty fields", map[string]any{"list": "l", "fields": map[string]any{}}},
		{"empty key", map[string]any{"list": "l", "fields": map[string]any{"": "b"}}},
		{"object value", map[string]any{"list": "l", "fields": map[string]any{"a": map[string]any{"b": 1}}}},
		{"too big", map[string]any{"list": "l", "fields": map[string]any{"a": strings.Repeat("y", 2001)}}},
	}
	for _, b := range bad {
		t.Run(b.name, func(t *testing.T) {
			b.args["cwd"] = dir
			out, err := memWorkStateWrite(db, b.args)
			if err != nil || !strings.HasPrefix(out, "Error: ") {
				t.Errorf("out %q err %v, want an Error: reply", out, err)
			}
		})
	}
	var n int
	db.QueryRow(`SELECT count(*) FROM work_state`).Scan(&n)
	if n != 1 {
		t.Errorf("rejected writes stored rows: count = %d", n)
	}
}

func TestMemWorkStateWriteTruncatesLongList(t *testing.T) {
	db, _ := openTestMemd(t)
	dir := t.TempDir()
	var out string
	for i := 0; i < 40; i++ {
		out = wsWrite(t, db, dir, "big", map[string]any{"task": "task number " + strings.Repeat("z", 60) + string(rune('A'+i%26)) + string(rune('a'+i/26)), "status": "todo"})
	}
	if len(out) > 1800 || !strings.Contains(out, "more line") || !strings.HasSuffix(out, "work_state_read shows them") {
		t.Errorf("reply not truncated (len %d):\n%s", len(out), out)
	}
}

func TestMemWorkStateHandoffReply(t *testing.T) {
	db, _ := openTestMemd(t)
	dir := t.TempDir()
	out := wsWrite(t, db, dir, "handoff", map[string]any{"text": strings.Repeat("h", 3000)})
	want := "Handoff stored for " + projectKey(dir) + "; the next session there sees it once, at startup."
	if out != want {
		t.Errorf("reply = %q, want %q", out, want)
	}
}
