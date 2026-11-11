package main

import (
	"bytes"
	"database/sql"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// resumeT0 is a fixed "now" so ages in the output are exact.
var resumeT0 = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

// seedResume builds three accounts' worth of sessions:
//
//	s-main    ~/.claude        "fix the parser bug", forked once
//	s-acct2   ~/.claude-work   summary "Terraform vpc refactor"
//	s-fork    ~/.claude        a fork of s-main, the newest
//	s-obs     an observer session (never listed)
//	agent-zz  a legacy bogus row that only a subagent transcript wrote (never listed)
func seedResume(t *testing.T) *sql.DB {
	t.Helper()
	db, _ := openTestMemd(t)
	ex := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	ts := func(h int) string { return resumeT0.Add(time.Duration(h) * time.Hour).Format("2006-01-02 15:04:05") }
	ex(`INSERT INTO sessions (session_id, project, cwd, is_observer, started_at, last_seen_at, forked_from) VALUES
	 ('s-main-1111', 'demo', '/work/demo', false, CAST(? AS TIMESTAMP), CAST(? AS TIMESTAMP), ''),
	 ('s-acct2-2222', 'infra', '/work/infra', false, CAST(? AS TIMESTAMP), CAST(? AS TIMESTAMP), ''),
	 ('s-fork-3333', 'demo', '/work/demo', false, CAST(? AS TIMESTAMP), CAST(? AS TIMESTAMP), 's-main-1111'),
	 ('s-obs-4444', 'demo', '/work/demo', true, CAST(? AS TIMESTAMP), CAST(? AS TIMESTAMP), ''),
	 ('agent-zz', 'demo', '/work/demo', false, CAST(? AS TIMESTAMP), CAST(? AS TIMESTAMP), '')`,
		ts(-10), ts(-9), ts(-8), ts(-7), ts(-3), ts(-2), ts(-1), ts(-1), ts(-1), ts(-1))
	ex(`INSERT INTO prompts (session_id, prompt_number, text, created_at) VALUES
	 ('s-main-1111', 1, 'fix the parser bug' || chr(10) || 'second line', CAST(? AS TIMESTAMP)),
	 ('s-main-1111', 2, 'now add a test', CAST(? AS TIMESTAMP)),
	 ('s-acct2-2222', 1, 'plan it', CAST(? AS TIMESTAMP)),
	 ('s-fork-3333', 1, 'fix the parser bug' || chr(10) || 'second line', CAST(? AS TIMESTAMP))`,
		ts(-10), ts(-9), ts(-8), ts(-3))
	ex(`INSERT INTO summaries (project, session_id, request, source) VALUES ('infra', 's-acct2-2222', 'Terraform vpc refactor', 'live')`)
	ex(`INSERT INTO transcript_offsets (path, session_id, sub, observer) VALUES
	 ('/home/u/.claude/projects/-work-demo/s-main-1111.jsonl', 's-main-1111', false, false),
	 ('/home/u/.claude-work/projects/-work-infra/s-acct2-2222.jsonl', 's-acct2-2222', false, false),
	 ('/home/u/.claude/projects/-work-demo/s-fork-3333.jsonl', 's-fork-3333', false, false),
	 ('/home/u/.claude/projects/-work-demo/s-obs-4444.jsonl', 's-obs-4444', false, true),
	 ('/home/u/.claude/projects/-work-demo/s-main-1111/subagents/agent-a1b2c3d4.jsonl', 's-main-1111', true, false),
	 ('/home/u/.claude/projects/-work-demo/subagents/agent-zz.jsonl', 'agent-zz', true, false)`)
	return db
}

func resumeIDs(items []resumeItem) []string {
	var ids []string
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	return ids
}

func TestResumeConfigDir(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"/home/u/.claude/projects/-w/s.jsonl", "/home/u/.claude"},
		{"/home/u/.claude-work/projects/-w/s.jsonl", "/home/u/.claude-work"},
		{"/home/u/projects/x/.claude/projects/-w/s.jsonl", "/home/u/projects/x/.claude"},
		{"/somewhere/else/s.jsonl", ""},
	} {
		if got := resumeConfigDir(tc.path); got != tc.want {
			t.Errorf("resumeConfigDir(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestResumeSearch(t *testing.T) {
	db := seedResume(t)
	for _, tc := range []struct {
		name, q string
		want    []string
	}{
		{"all, newest first, no observer or subagent rows", "", []string{"s-fork-3333", "s-acct2-2222", "s-main-1111"}},
		{"prompt text, any case", "PARSER", []string{"s-fork-3333", "s-main-1111"}},
		{"later line of a prompt", "second line", []string{"s-fork-3333", "s-main-1111"}},
		{"summary request", "vpc", []string{"s-acct2-2222"}},
		{"project", "infra", []string{"s-acct2-2222"}},
		{"every term must match", "parser vpc", nil},
		{"a percent sign is literal", "%", nil},
		{"an underscore is literal", "s_main", nil},
		{"no match", "kubernetes", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resumeSearch(db, tc.q, 20)
			if err != nil {
				t.Fatal(err)
			}
			if g := strings.Join(resumeIDs(got), ","); g != strings.Join(tc.want, ",") {
				t.Errorf("ids = %q, want %q", g, strings.Join(tc.want, ","))
			}
		})
	}
	t.Run("limit", func(t *testing.T) {
		got, err := resumeSearch(db, "", 1)
		if err != nil || len(got) != 1 || got[0].ID != "s-fork-3333" {
			t.Errorf("limit 1 = %v, %v", resumeIDs(got), err)
		}
	})
	t.Run("fields", func(t *testing.T) {
		got, err := resumeSearch(db, "", 20)
		if err != nil {
			t.Fatal(err)
		}
		by := map[string]resumeItem{}
		for _, it := range got {
			by[it.ID] = it
		}
		m := by["s-main-1111"]
		if m.Title != "fix the parser bug" || m.Project != "demo" || m.Cwd != "/work/demo" ||
			m.ConfigDir != "/home/u/.claude" || m.Prompts != 2 || m.Forks != 1 || m.ForkedFrom != "" {
			t.Errorf("s-main = %+v", m)
		}
		if a := by["s-acct2-2222"]; a.Title != "plan it" || a.ConfigDir != "/home/u/.claude-work" {
			t.Errorf("s-acct2 = %+v", a)
		}
		if f := by["s-fork-3333"]; f.ForkedFrom != "s-main-1111" || f.Forks != 0 {
			t.Errorf("s-fork = %+v", f)
		}
	})
	t.Run("title falls back to the summary request", func(t *testing.T) {
		if _, err := db.Exec(`DELETE FROM prompts WHERE session_id = 's-acct2-2222'`); err != nil {
			t.Fatal(err)
		}
		got, err := resumeSearch(db, "vpc", 20)
		if err != nil || len(got) != 1 || got[0].Title != "Terraform vpc refactor" {
			t.Errorf("got %+v, %v", got, err)
		}
	})
}

func TestResumeResolve(t *testing.T) {
	db := seedResume(t)
	t.Run("exact and prefix", func(t *testing.T) {
		for _, ref := range []string{"s-main-1111", "s-main", "S-MAIN"} {
			it, via, err := resumeResolve(db, ref)
			if err != nil || it.ID != "s-main-1111" || via != "" {
				t.Errorf("resolve(%q) = %q, %q, %v", ref, it.ID, via, err)
			}
		}
	})
	t.Run("ambiguous prefix", func(t *testing.T) {
		_, _, err := resumeResolve(db, "s-")
		var amb *resumeAmbiguous
		if !errors.As(err, &amb) || len(amb.IDs) != 3 {
			t.Errorf("err = %v, want ambiguous over 3 ids", err)
		}
	})
	t.Run("a subagent id resolves to its parent", func(t *testing.T) {
		for _, ref := range []string{"a1b2c3d4", "agent-a1b2c3d4", "a1b2"} {
			it, via, err := resumeResolve(db, ref)
			if err != nil || it.ID != "s-main-1111" || via != "a1b2c3d4" {
				t.Errorf("resolve(%q) = %q, %q, %v", ref, it.ID, via, err)
			}
		}
	})
	t.Run("a legacy agent-named session row is not resumable", func(t *testing.T) {
		if _, _, err := resumeResolve(db, "agent-zz"); !errors.Is(err, errResumeNotFound) {
			t.Errorf("err = %v, want not found", err)
		}
	})
	t.Run("observer and unknown", func(t *testing.T) {
		for _, ref := range []string{"s-obs-4444", "nope"} {
			if _, _, err := resumeResolve(db, ref); !errors.Is(err, errResumeNotFound) {
				t.Errorf("resolve(%q) err = %v, want not found", ref, err)
			}
		}
	})
}

func TestResumeCommand(t *testing.T) {
	dirs := map[string]bool{"/work/demo": true, "/work/with space": true, "/work/it's $HOME `x`": true}
	exists := func(p string) bool { return dirs[p] }
	base := resumeItem{ID: "s-main-1111", Cwd: "/work/demo", ConfigDir: "/home/u/.claude"}
	with := func(f func(*resumeItem)) resumeItem { it := base; f(&it); return it }
	for _, tc := range []struct {
		name     string
		it       resumeItem
		fork     bool
		envCfg   string
		want     string
		wantNote string
	}{
		{"default dir is omitted", base, false, "", "cd -- /work/demo && claude --resume s-main-1111", ""},
		{"fork", base, true, "", "cd -- /work/demo && claude --resume s-main-1111 --fork-session", ""},
		{"other account", with(func(i *resumeItem) { i.ConfigDir = "/home/u/.claude-work" }), false, "",
			"cd -- /work/demo && CLAUDE_CONFIG_DIR=/home/u/.claude-work claude --resume s-main-1111", ""},
		{"default dir is spelled out when the shell points elsewhere", base, false, "/home/u/.claude-work",
			"cd -- /work/demo && CLAUDE_CONFIG_DIR=/home/u/.claude claude --resume s-main-1111", ""},
		{"shell already at the default", base, false, "/home/u/.claude", "cd -- /work/demo && claude --resume s-main-1111", ""},
		{"space in cwd", with(func(i *resumeItem) { i.Cwd = "/work/with space" }), false, "",
			"cd -- '/work/with space' && claude --resume s-main-1111", ""},
		{"quote, dollar and backtick in cwd", with(func(i *resumeItem) { i.Cwd = "/work/it's $HOME `x`" }), false, "",
			`cd -- '/work/it'\''s $HOME ` + "`x`" + `' && claude --resume s-main-1111`, ""},
		{"config dir with a space", with(func(i *resumeItem) { i.ConfigDir = "/home/u/my claude" }), false, "",
			"cd -- /work/demo && CLAUDE_CONFIG_DIR='/home/u/my claude' claude --resume s-main-1111", ""},
		{"hostile session id is quoted", with(func(i *resumeItem) { i.ID = "x; rm -rf ~" }), false, "",
			"cd -- /work/demo && claude --resume 'x; rm -rf ~'", ""},
		{"cwd gone", with(func(i *resumeItem) { i.Cwd = "/work/gone" }), false, "",
			"claude --resume s-main-1111", "/work/gone no longer exists"},
		{"no cwd recorded", with(func(i *resumeItem) { i.Cwd = "" }), false, "",
			"claude --resume s-main-1111", "no working directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, note := resumeCommand(tc.it, resumeCmdOpts{Fork: tc.fork, Home: "/home/u", EnvConfigDir: tc.envCfg, DirExists: exists})
			if got != tc.want {
				t.Errorf("command:\n got %s\nwant %s", got, tc.want)
			}
			if (tc.wantNote == "") != (note == "") || !strings.Contains(note, tc.wantNote) {
				t.Errorf("note = %q, want it to contain %q", note, tc.wantNote)
			}
		})
	}
}

// The printed command has to survive a real shell: run it with sh and check what claude saw.
func TestResumeCommandRunsInAShell(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "it's a $HOME dir")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	stub := "#!/bin/sh\nprintf '%s|%s|%s|%s' \"$PWD\" \"$CLAUDE_CONFIG_DIR\" \"$1\" \"$2\"\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(root, "acct two")
	cmd, _ := resumeCommand(resumeItem{ID: "sid-1", Cwd: cwd, ConfigDir: cfg}, resumeCmdOpts{Home: "/nonexistent-home", DirExists: func(string) bool { return true }})
	c := exec.Command("sh", "-c", cmd)
	c.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
	got, err := c.Output()
	if err != nil {
		t.Fatalf("sh -c %q: %v", cmd, err)
	}
	want := cwd + "|" + cfg + "|--resume|sid-1"
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		want = resolved + "|" + cfg + "|--resume|sid-1"
	}
	if string(got) != want {
		t.Errorf("shell saw %q, want %q", got, want)
	}
}

func TestResumeAgo(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{20 * time.Second, "just now"}, {5 * time.Minute, "5m ago"}, {3 * time.Hour, "3h ago"},
		{49 * time.Hour, "2d ago"}, {20 * 24 * time.Hour, "2w ago"}, {90 * 24 * time.Hour, "3mo ago"},
	} {
		if got := resumeAgo(resumeT0, resumeT0.Add(-tc.d)); got != tc.want {
			t.Errorf("resumeAgo(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestResumeRenderGroupsForks(t *testing.T) {
	items := []resumeItem{
		{ID: "s-fork-3333", Title: "fork title", ForkedFrom: "s-main-1111", LastSeen: resumeT0.Add(-1 * time.Hour), ConfigDir: "/home/u/.claude"},
		{ID: "s-acct2-2222", Title: "plan it", LastSeen: resumeT0.Add(-2 * time.Hour), ConfigDir: "/home/u/.claude-work", Project: "infra"},
		{ID: "s-main-1111", Title: "fix the parser bug", LastSeen: resumeT0.Add(-9 * time.Hour), ConfigDir: "/home/u/.claude", Forks: 1, Project: "demo"},
		{ID: "s-lost-5555", Title: "orphan fork", ForkedFrom: "not-in-results", LastSeen: resumeT0.Add(-20 * time.Hour)},
	}
	var out bytes.Buffer
	order := renderResumeList(&out, items, resumeT0, "/home/u")
	// The group of s-main (its fork is the newest thing in it) sorts first; the fork follows
	// its original; an orphan fork stands alone.
	if got, want := strings.Join(resumeIDs(order), ","), "s-main-1111,s-fork-3333,s-acct2-2222,s-lost-5555"; got != want {
		t.Fatalf("order = %s, want %s\n%s", got, want, out.String())
	}
	text := out.String()
	for _, want := range []string{"1) fix the parser bug", "(1 fork)", "2) fork title", "3) plan it", "~/.claude-work", "9h ago", "4) orphan fork"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, "2) fork title") && !strings.HasPrefix(l, "    ") {
			t.Errorf("fork line is not indented: %q", l)
		}
		if strings.Contains(l, "1) fix") && strings.HasPrefix(l, " ") {
			t.Errorf("original line is indented: %q", l)
		}
	}
}

func resumeDaemon(t *testing.T, db *sql.DB) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "m.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: memdHandler(db)}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}

func TestResumeCommandThroughDaemon(t *testing.T) {
	db := seedResume(t)
	sock := resumeDaemon(t, db)
	cwd := t.TempDir()
	if _, err := db.Exec(`UPDATE sessions SET cwd = ? WHERE session_id = 's-main-1111'`, cwd); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", "/home/u")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	run := func(args []string, in string, tty bool) (int, string, string) {
		var out, errw bytes.Buffer
		code := memResumeTo(args, sock, strings.NewReader(in), tty, &out, &errw)
		return code, out.String(), errw.String()
	}

	t.Run("id prints the command", func(t *testing.T) {
		code, out, errw := run([]string{"s-main-1111"}, "", false)
		want := "cd -- " + shellQuote(cwd) + " && claude --resume s-main-1111\n"
		if code != 0 || out != want {
			t.Errorf("code %d out %q want %q (stderr %q)", code, out, want, errw)
		}
	})
	t.Run("--fork after the id", func(t *testing.T) {
		code, out, _ := run([]string{"s-main", "--fork"}, "", false)
		if code != 0 || !strings.HasSuffix(out, "claude --resume s-main-1111 --fork-session\n") {
			t.Errorf("code %d out %q", code, out)
		}
	})
	t.Run("other account gets CLAUDE_CONFIG_DIR", func(t *testing.T) {
		_, out, errw := run([]string{"s-acct2"}, "", false)
		if !strings.Contains(out, "CLAUDE_CONFIG_DIR=/home/u/.claude-work claude --resume s-acct2-2222") {
			t.Errorf("out %q", out)
		}
		if !strings.Contains(errw, "no longer exists") { // /work/infra does not exist here
			t.Errorf("stderr %q lacks the missing-directory note", errw)
		}
		if strings.Contains(out, "cd --") {
			t.Errorf("out %q must not cd into a missing directory", out)
		}
	})
	t.Run("subagent id resolves to the parent, with a note", func(t *testing.T) {
		code, out, errw := run([]string{"a1b2c3d4"}, "", false)
		if code != 0 || !strings.Contains(out, "claude --resume s-main-1111") || !strings.Contains(errw, "subagent a1b2c3d4") {
			t.Errorf("code %d out %q stderr %q", code, out, errw)
		}
	})
	t.Run("ambiguous prefix fails and lists the ids", func(t *testing.T) {
		code, out, errw := run([]string{"s-"}, "", false)
		if code != 1 || out != "" || !strings.Contains(errw, "s-main-1111") || !strings.Contains(errw, "ambiguous") {
			t.Errorf("code %d out %q stderr %q", code, out, errw)
		}
	})
	t.Run("--list shows sessions, not subagents", func(t *testing.T) {
		code, out, _ := run([]string{"--list"}, "", false)
		if code != 0 || !strings.Contains(out, "fix the parser bug") || !strings.Contains(out, "(1 fork)") ||
			strings.Contains(out, "agent-") || strings.Contains(out, "s-obs") {
			t.Errorf("code %d out:\n%s", code, out)
		}
	})
	t.Run("--list with a query", func(t *testing.T) {
		code, out, _ := run([]string{"--list", "vpc"}, "", false)
		if code != 0 || !strings.Contains(out, "plan it") || strings.Contains(out, "fix the parser bug") {
			t.Errorf("code %d out:\n%s", code, out)
		}
	})
	t.Run("a query that is not an id lists on a pipe", func(t *testing.T) {
		code, out, _ := run([]string{"parser"}, "", false)
		if code != 0 || !strings.Contains(out, "fix the parser bug") || strings.Contains(out, "claude --resume") {
			t.Errorf("code %d out:\n%s", code, out)
		}
	})
	t.Run("a query on a terminal picks by number", func(t *testing.T) {
		code, out, errw := run([]string{"parser"}, "2\n", true)
		// Matches newest first: the group is s-main then its fork s-fork-3333.
		if code != 0 || !strings.HasSuffix(out, "claude --resume s-fork-3333\n") || !strings.Contains(errw, "fix the parser bug") {
			t.Errorf("code %d out %q stderr:\n%s", code, out, errw)
		}
		code, out, _ = run([]string{"parser"}, "9\n", true)
		if code == 0 || out != "" {
			t.Errorf("out of range pick: code %d out %q", code, out)
		}
	})
	t.Run("no match", func(t *testing.T) {
		if code, out, errw := run([]string{"kubernetes"}, "", false); code != 1 || out != "" || !strings.Contains(errw, "no sessions") {
			t.Errorf("code %d out %q stderr %q", code, out, errw)
		}
	})
	t.Run("daemon down", func(t *testing.T) {
		var out, errw bytes.Buffer
		code := memResumeTo([]string{"x"}, filepath.Join(t.TempDir(), "nope.sock"), strings.NewReader(""), false, &out, &errw)
		if code != 1 || !strings.Contains(errw.String(), "memory serve") {
			t.Errorf("code %d stderr %q", code, errw.String())
		}
	})
}
