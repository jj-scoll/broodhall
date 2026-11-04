package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// sweepFx is one git checkout shared by simulated sessions. Sessions are only session ids:
// each edit goes through the real record-edit hook and each command through the real
// pre-tool-use hook, with its own state dir, so the tests share nothing with the machine.
type sweepFx struct {
	t     *testing.T
	state string
	repo  string
}

const (
	sessA = "aaaaaaaa-1111-2222-3333-444444444444"
	sessB = "bbbbbbbb-1111-2222-3333-444444444444"
)

func gitEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
}

func newSweepFx(t *testing.T) *sweepFx {
	t.Helper()
	gitEnv(t)
	state := t.TempDir()
	t.Setenv("HANDOFF_STATE_DIR", state)
	t.Setenv("HOME", t.TempDir())
	// The git family also asks about stash, checkout . and friends; switch it off so every
	// ask in these tests is the sweep guard's.
	t.Setenv("CLAUDE_TOOLS_OFF", "git")
	t.Setenv("CLAUDE_TOOLS_OBSERVE", "")
	root, _ := filepath.EvalSymlinks(t.TempDir())
	f := &sweepFx{t: t, state: state, repo: filepath.Join(root, "repo")}
	os.MkdirAll(f.repo, 0o755)
	f.git(f.repo, "init", "-q", "-b", "main")
	f.write("a.txt", "a\n")
	f.write("b.txt", "b\n")
	f.write("sp ace/c d.txt", "c\n")
	f.write("docs/d.txt", "d\n")
	f.git(f.repo, "add", "-A")
	f.git(f.repo, "commit", "-q", "-m", "init")
	return f
}

func (f *sweepFx) git(dir string, args ...string) string {
	f.t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

func (f *sweepFx) write(rel, content string) string {
	f.t.Helper()
	p := filepath.Join(f.repo, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return p
}

// edit has session s change a file through the Write tool: the file changes on disk and the
// PostToolUse hook runs.
func (f *sweepFx) edit(s, rel, content string) string {
	f.t.Helper()
	p := f.write(rel, content)
	f.record(s, "Write", "file_path", p)
	return p
}

func (f *sweepFx) record(s, tool, key, p string) {
	f.t.Helper()
	in, _ := json.Marshal(map[string]any{"hook_event_name": "PostToolUse", "session_id": s, "cwd": f.repo,
		"tool_name": tool, "tool_input": map[string]any{key: p}, "tool_response": map[string]any{}})
	if code, out, errOut := runHook(f.t, recordEdit, string(in)); code != 0 || out != "" || errOut != "" {
		f.t.Fatalf("record-edit: code %d stdout %q stderr %q", code, out, errOut)
	}
}

// bash runs a Bash command through the pre-tool-use hook as session s in cwd and returns
// the permission reason ("" when the hook did not ask) and the ledger for s.
func (f *sweepFx) bash(s, cwd, cmd string) (reason string) {
	f.t.Helper()
	payload := bashInput("PreToolUse", s, cwd, cmd, "")
	var stdout, stderr bytes.Buffer
	code := preToolUseRun([]byte(payload), "CT", func([]byte) ([]byte, []byte, int) { return nil, nil, 0 }, &stdout, &stderr)
	if code != 0 {
		f.t.Fatalf("%q: exit %d stderr %q", cmd, code, stderr.String())
	}
	var r struct {
		H struct {
			Decision string `json:"permissionDecision"`
			Reason   string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if stdout.Len() > 0 && json.Unmarshal(stdout.Bytes(), &r) == nil && r.H.Decision == "ask" {
		return r.H.Reason
	}
	return ""
}

func (f *sweepFx) ledger(s string) string {
	b, _ := os.ReadFile(filepath.Join(f.state, "claude-handoff", s+".events"))
	return string(b)
}

func TestSweepAsksForAnotherSessionsChange(t *testing.T) {
	cmds := []string{
		"git add -A", "git add .", "git add -u", "git add --all", "git add -A -- .",
		"git commit -a -m wip", "git commit -am wip", "git commit -qam wip", "git commit --all -m wip",
		"git stash", "git stash push", "git stash save wip", "git stash -u", "git stash push -m wip",
		"git checkout .", "git checkout -- .", "git restore .", "git reset --hard", "git reset --hard HEAD",
		"bash -c 'git add -A'", "sudo git add -A", "git status && git add -A", "echo hi | (git add -A)",
		"git -C {repo} add -A",
	}
	for _, cmd := range cmds {
		t.Run(cmd, func(t *testing.T) {
			f := newSweepFx(t)
			f.edit(sessA, "a.txt", "changed by A\n")
			got := f.bash(sessB, f.repo, strings.ReplaceAll(cmd, "{repo}", f.repo))
			for _, want := range []string{"aaaaaaaa", "a.txt", "ASK:"} {
				if !strings.Contains(got, want) {
					t.Errorf("reason lacks %q: %q", want, got)
				}
			}
			if strings.Contains(got, "bbbbbbbb") {
				t.Errorf("reason names the asking session: %q", got)
			}
			if !strings.Contains(f.ledger(sessB), "ask\tshared-tree-sweep") {
				t.Errorf("ledger: %q", f.ledger(sessB))
			}
		})
	}
}

func TestSweepUntrackedFile(t *testing.T) {
	f := newSweepFx(t)
	f.edit(sessA, "new.txt", "new\n") // a Write that creates a file
	for cmd, want := range map[string]bool{
		"git add -A": true, "git add .": true, "git clean -f": true, "git clean -fd": true,
		"git clean -n": false, "git clean --dry-run -f": false,
		"git add -u": false, "git commit -am x": false, "git stash": false, "git reset --hard": false,
		"git checkout .": false, "git stash -u": true,
	} {
		if got := f.bash(sessB, f.repo, cmd) != ""; got != want {
			t.Errorf("%q asked=%v, want %v", cmd, got, want)
		}
	}
}

func TestSweepSilent(t *testing.T) {
	setup := func(f *sweepFx) {}
	tests := []struct {
		name  string
		setup func(f *sweepFx)
		who   string // session that runs cmd
		cwd   string // relative to the repo; "" is the root
		cmd   string
	}{
		{"own file only", func(f *sweepFx) { f.edit(sessB, "a.txt", "mine\n") }, sessB, "", "git add -A"},
		{"committed tree", func(f *sweepFx) {
			f.edit(sessA, "a.txt", "x\n")
			f.git(f.repo, "commit", "-qam", "A commits")
		}, sessB, "", "git add -A"},
		{"specific path, other session's file", func(f *sweepFx) { f.edit(sessA, "a.txt", "x\n") }, sessB, "", "git add a.txt"},
		{"specific path, own file", func(f *sweepFx) { f.edit(sessA, "a.txt", "x\n"); f.edit(sessB, "b.txt", "y\n") }, sessB, "", "git add b.txt"},
		{"add -p style specific", func(f *sweepFx) { f.edit(sessA, "a.txt", "x\n") }, sessB, "", "git add -- b.txt docs"},
		{"stash limited to own file", func(f *sweepFx) { f.edit(sessA, "a.txt", "x\n"); f.edit(sessB, "b.txt", "y\n") }, sessB, "", "git stash push -- b.txt"},
		{"stash push own file no dashes", func(f *sweepFx) { f.edit(sessA, "a.txt", "x\n"); f.edit(sessB, "b.txt", "y\n") }, sessB, "", "git stash push -m wip b.txt"},
		{"stash list", func(f *sweepFx) { f.edit(sessA, "a.txt", "x\n") }, sessB, "", "git stash list"},
		{"stash pop", func(f *sweepFx) { f.edit(sessA, "a.txt", "x\n") }, sessB, "", "git stash pop"},
		{"commit without -a", func(f *sweepFx) { f.edit(sessA, "a.txt", "x\n") }, sessB, "", "git commit -m x"},
		{"commit -m text containing a", func(f *sweepFx) { f.edit(sessA, "a.txt", "x\n") }, sessB, "", `git commit -m"all about a"`},
		{"cwd subdir does not hold the change", func(f *sweepFx) { f.edit(sessA, "a.txt", "x\n") }, sessB, "docs", "git checkout ."},
		{"cwd subdir add .", func(f *sweepFx) { f.edit(sessA, "a.txt", "x\n") }, sessB, "docs", "git add ."},
		{"git add -n", func(f *sweepFx) { f.edit(sessA, "a.txt", "x\n") }, sessB, "", "git add -An"},
		{"restore staged only", func(f *sweepFx) { f.edit(sessA, "a.txt", "x\n") }, sessB, "", "git restore --staged ."},
		{"bash-made change is not recorded", func(f *sweepFx) { f.write("a.txt", "sed -i by A\n") }, sessB, "", "git add -A"},
		{"no other session", setup, sessB, "", "git add -A"},
		{"other session recorded a clean file", func(f *sweepFx) { f.record(sessA, "Edit", "file_path", filepath.Join(f.repo, "a.txt")) }, sessB, "", "git add -A"},
		{"other repo state only", func(f *sweepFx) { f.edit(sessA, "a.txt", "x\n") }, sessB, "", "git diff"},
		{"retargeted git dir", func(f *sweepFx) { f.edit(sessA, "a.txt", "x\n") }, sessB, "", "git --work-tree=/elsewhere add -A"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newSweepFx(t)
			tt.setup(f)
			if got := f.bash(tt.who, filepath.Join(f.repo, tt.cwd), tt.cmd); got != "" {
				t.Errorf("%q asked: %q", tt.cmd, got)
			}
		})
	}
}

func TestSweepBothSessionsEditedTheFile(t *testing.T) {
	f := newSweepFx(t)
	f.edit(sessA, "a.txt", "A\n")
	f.edit(sessB, "a.txt", "A and B\n")
	if got := f.bash(sessB, f.repo, "git add -A"); !strings.Contains(got, "aaaaaaaa") {
		t.Errorf("a file both sessions edited holds A's lines too: %q", got)
	}
}

func TestSweepPathsWithSpaces(t *testing.T) {
	f := newSweepFx(t)
	f.edit(sessA, "sp ace/c d.txt", "x\n")
	f.edit(sessA, "sp ace/new file.txt", "y\n")
	got := f.bash(sessB, f.repo, "git add -A")
	for _, want := range []string{"sp ace/c d.txt", "sp ace/new file.txt"} {
		if !strings.Contains(got, want) {
			t.Errorf("reason lacks %q: %q", want, got)
		}
	}
	if got := f.bash(sessB, f.repo, `git add "sp ace/c d.txt"`); got != "" {
		t.Errorf("specific path asked: %q", got)
	}
	if got := f.bash(sessB, f.repo, `git stash push -- "sp ace/c d.txt"`); !strings.Contains(got, "c d.txt") {
		t.Errorf("stash of A's file with a space did not ask: %q", got)
	}
}

func TestSweepListsAtMostFiveFiles(t *testing.T) {
	f := newSweepFx(t)
	for _, n := range []string{"f1", "f2", "f3", "f4", "f5", "f6", "f7"} {
		f.edit(sessA, n+".txt", "x\n")
	}
	got := f.bash(sessB, f.repo, "git add -A")
	if n := strings.Count(got, ".txt"); n != 5 {
		t.Errorf("lists %d files, want 5: %q", n, got)
	}
	if !strings.Contains(got, "+2 more") {
		t.Errorf("no count of the rest: %q", got)
	}
}

func TestSweepNamesEverySession(t *testing.T) {
	f := newSweepFx(t)
	f.edit(sessA, "a.txt", "A\n")
	f.edit("cccccccc-1111", "b.txt", "C\n")
	got := f.bash(sessB, f.repo, "git add -A")
	for _, want := range []string{"aaaaaaaa", "cccccccc", "a.txt", "b.txt"} {
		if !strings.Contains(got, want) {
			t.Errorf("reason lacks %q: %q", want, got)
		}
	}
}

func TestSweepStaleSession(t *testing.T) {
	f := newSweepFx(t)
	f.edit(sessA, "a.txt", "x\n")
	if f.bash(sessB, f.repo, "git add -A") == "" {
		t.Fatal("live session: want an ask")
	}
	old := time.Now().Add(-sweepWindow - time.Minute)
	for _, ext := range []string{".edits", ".guard", ".events"} {
		os.Chtimes(filepath.Join(f.state, "claude-handoff", sessA+ext), old, old)
	}
	if got := f.bash(sessB, f.repo, "git add -A"); got != "" {
		t.Errorf("stale session asked: %q", got)
	}
	// Any later sign of life makes it live again.
	f.edit(sessA, "b.txt", "y\n")
	if f.bash(sessB, f.repo, "git add -A") == "" {
		t.Error("session active again: want an ask")
	}
}

func TestSweepLinkedWorktreeIsAnotherCheckout(t *testing.T) {
	f := newSweepFx(t)
	wt := filepath.Join(filepath.Dir(f.repo), "wt")
	f.git(f.repo, "worktree", "add", "-q", "-b", "side", wt)
	// A edits the main checkout, B sweeps the linked worktree.
	f.edit(sessA, "a.txt", "x\n")
	if got := f.bash(sessB, wt, "git add -A"); got != "" {
		t.Errorf("B in the worktree asked about main's file: %q", got)
	}
	// The same file name edited inside the worktree is a different file.
	p := filepath.Join(wt, "b.txt")
	os.WriteFile(p, []byte("wt\n"), 0o644)
	f.record(sessA, "Write", "file_path", p)
	if got := f.bash(sessB, f.repo, "git commit -am x"); strings.Contains(got, "b.txt") {
		t.Errorf("B in main asked about the worktree's file: %q", got)
	}
	if got := f.bash(sessB, wt, "git add -A"); !strings.Contains(got, "b.txt") {
		t.Errorf("two sessions in one worktree: want an ask, got %q", got)
	}
}

func TestSweepFileOutsideRepo(t *testing.T) {
	f := newSweepFx(t)
	out := filepath.Join(t.TempDir(), "notes.txt")
	os.WriteFile(out, []byte("x"), 0o644)
	f.record(sessA, "Write", "file_path", out)
	if got := f.bash(sessB, f.repo, "git add -A"); got != "" {
		t.Errorf("asked about a file outside the repo: %q", got)
	}
}

func TestSweepNotARepo(t *testing.T) {
	f := newSweepFx(t)
	f.edit(sessA, "a.txt", "x\n")
	if got := f.bash(sessB, t.TempDir(), "git add -A"); got != "" {
		t.Errorf("asked outside a repo: %q", got)
	}
}

func TestSweepNotebookAndMultiEdit(t *testing.T) {
	f := newSweepFx(t)
	nb := f.write("n.ipynb", "{}")
	f.record(sessA, "NotebookEdit", "notebook_path", nb)
	if got := f.bash(sessB, f.repo, "git add -A"); !strings.Contains(got, "n.ipynb") {
		t.Errorf("notebook edit not recorded: %q", got)
	}
}

func TestSweepSwitches(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		f := newSweepFx(t)
		f.edit(sessA, "a.txt", "x\n")
		t.Setenv("CLAUDE_TOOLS_OFF", "git,sweep")
		if got := f.bash(sessB, f.repo, "git add -A"); got != "" {
			t.Errorf("sweep off: %q", got)
		}
	})
	t.Run("observe", func(t *testing.T) {
		f := newSweepFx(t)
		f.edit(sessA, "a.txt", "x\n")
		t.Setenv("CLAUDE_TOOLS_OBSERVE", "1")
		if got := f.bash(sessB, f.repo, "git add -A"); got != "" {
			t.Errorf("observe: still asked: %q", got)
		}
		if l := f.ledger(sessB); !strings.Contains(l, "would-ask\tshared-tree-sweep") || strings.Contains(l, "\task\t") {
			t.Errorf("ledger: %q", l)
		}
	})
	t.Run("git family on: sweep reason wins over the generic ask", func(t *testing.T) {
		f := newSweepFx(t)
		f.edit(sessA, "a.txt", "x\n")
		t.Setenv("CLAUDE_TOOLS_OFF", "")
		if got := f.bash(sessB, f.repo, "git stash"); !strings.Contains(got, "aaaaaaaa") {
			t.Errorf("want the sweep reason: %q", got)
		}
		// With nobody else involved the generic ask is unchanged.
		f2 := newSweepFx(t)
		t.Setenv("CLAUDE_TOOLS_OFF", "")
		if got := f2.bash(sessB, f2.repo, "git stash"); !strings.Contains(got, "git stash moves") {
			t.Errorf("want the generic ask: %q", got)
		}
	})
}

func TestRecordEdit(t *testing.T) {
	f := newSweepFx(t)
	read := func() string {
		b, _ := os.ReadFile(filepath.Join(f.state, "claude-handoff", sessA+".edits"))
		return string(b)
	}
	// Read tools and unsafe session ids record nothing.
	in, _ := json.Marshal(map[string]any{"session_id": sessA, "cwd": f.repo, "tool_name": "Read", "tool_input": map[string]any{"file_path": filepath.Join(f.repo, "a.txt")}})
	runHook(t, recordEdit, string(in))
	in, _ = json.Marshal(map[string]any{"session_id": "../evil", "cwd": f.repo, "tool_name": "Write", "tool_input": map[string]any{"file_path": filepath.Join(f.repo, "a.txt")}})
	runHook(t, recordEdit, string(in))
	if got := read(); got != "" {
		t.Fatalf("recorded %q", got)
	}
	if _, err := os.Stat(filepath.Join(f.state, "evil.edits")); err == nil {
		t.Fatal("unsafe id wrote a file")
	}
	// A relative path resolves against the hook's cwd; a symlinked directory resolves to the real one.
	link := filepath.Join(t.TempDir(), "link")
	os.Symlink(f.repo, link)
	in, _ = json.Marshal(map[string]any{"session_id": sessA, "cwd": link, "tool_name": "Edit", "tool_input": map[string]any{"file_path": "a.txt"}})
	if code, out, _ := runHook(t, recordEdit, string(in)); code != 0 || out != "" {
		t.Fatalf("code %d out %q", code, out)
	}
	if got := read(); !strings.Contains(got, strconv.Quote(filepath.Join(f.repo, "a.txt"))) {
		t.Errorf("edits file %q lacks the real path", got)
	}
	// Garbage input never fails the hook.
	if code, _, _ := runHook(t, recordEdit, "not json"); code != 0 {
		t.Errorf("garbage input: exit %d", code)
	}
}

// sweepOf reads each sweeping form and nothing else.
func TestSweepOf(t *testing.T) {
	tests := []struct {
		cmd  string
		want string // label, "" for not a sweep
	}{
		{"git add -A", "git add -A"}, {"git add --all", "git add -A"}, {"git add -u", "git add -u"}, {"git add -u .", "git add -u"},
		{"git add .", "git add ."}, {"git add ./", "git add ."}, {"git add -v .", "git add ."}, {"git add -A src", "git add -A"},
		{"git add a.go", ""}, {"git add -f a.go", ""}, {"git add -n .", ""}, {"git add --dry-run -A", ""}, {"git add", ""},
		{"git commit -a", "git commit -a"}, {"git commit -am x", "git commit -a"}, {"git commit --all", "git commit -a"},
		{"git commit -m x", ""}, {"git commit -m 'a -a'", ""}, {"git commit --amend --no-edit", ""}, {"git commit -a --dry-run", ""},
		{"git stash", "git stash"}, {"git stash push", "git stash"}, {"git stash save x", "git stash"}, {"git stash -u", "git stash"},
		{"git stash list", ""}, {"git stash pop", ""}, {"git stash apply", ""}, {"git stash drop", ""}, {"git stash show", ""},
		{"git checkout .", "git checkout ."}, {"git checkout -- .", "git checkout ."}, {"git checkout HEAD .", "git checkout ."},
		{"git checkout main", ""}, {"git checkout -b x", ""}, {"git checkout -- a.go", ""},
		{"git restore .", "git restore ."}, {"git restore --worktree .", "git restore ."}, {"git restore --staged .", ""}, {"git restore a.go", ""},
		{"git reset --hard", "git reset --hard"}, {"git reset --soft HEAD~1", ""}, {"git reset", ""},
		{"git clean -f", "git clean -f"}, {"git clean -fdx", "git clean -f"}, {"git clean --force", "git clean -f"},
		{"git clean -n", ""}, {"git clean -fn", ""}, {"git clean -d", ""},
		{"git status", ""}, {"git diff", ""}, {"git log", ""},
	}
	for _, tt := range tests {
		cmds, ok := parseShell(tt.cmd)
		if !ok {
			t.Fatalf("parse %q", tt.cmd)
		}
		var got string
		if s := findSweeps(cmds, "/w", "/h"); len(s) > 0 {
			got = s[0].Label
		}
		if got != tt.want {
			t.Errorf("%q: %q, want %q", tt.cmd, got, tt.want)
		}
	}
}

func TestSweepFollowsDirectory(t *testing.T) {
	for cmd, want := range map[string]string{
		"git add -A":                      "/w",
		"git -C sub add -A":               "/w/sub",
		"git -C /elsewhere add -A":        "/elsewhere",
		"cd sub && git add -A":            "/w/sub",
		"cd /x && git -C y add -A":        "/x/y",
		"GIT_DIR=/x git add -A":           "",
		"git --git-dir=/x add -A":         "",
		"git --work-tree /x add -A":       "",
		"env GIT_WORK_TREE=/x git add -A": "",
	} {
		cmds, _ := parseShell(cmd)
		s := findSweeps(cmds, "/w", "/h")
		switch {
		case want == "" && len(s) != 0:
			t.Errorf("%q: swept despite a retargeted git", cmd)
		case want != "" && (len(s) != 1 || s[0].Dir != want):
			t.Errorf("%q: %+v, want dir %s", cmd, s, want)
		}
	}
}

func TestParseStatusZ(t *testing.T) {
	got := parseStatusZ([]byte(" M a.txt\x00?? sp ace/new file\x00R  new.txt\x00old.txt\x00MM b.txt\x00"))
	want := []statusEntry{{" M", "a.txt"}, {"??", "sp ace/new file"}, {"R ", "new.txt"}, {"MM", "b.txt"}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d: %v, want %v", i, got[i], want[i])
		}
	}
}

func TestPruneEdits(t *testing.T) {
	t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
	now := time.Now()
	appendEdit(sessA, "/r/old.txt", now.Add(-48*time.Hour))
	for i := 0; i < 3000; i++ {
		appendEdit(sessA, "/r/same.txt", now)
	}
	for i := 0; i < 3000; i++ { // push past the prune size
		appendEdit(sessA, "/r/a/very/long/path/to/make/the/file/grow/quickly/file-"+strconv.Itoa(i%3)+".txt", now)
	}
	got := readEdits(editsPath(sessA))
	if _, ok := got["/r/old.txt"]; ok {
		t.Error("two-day-old entry survived the prune")
	}
	if _, ok := got["/r/same.txt"]; !ok {
		t.Error("recent entry lost")
	}
	if st, _ := os.Stat(editsPath(sessA)); st.Size() > 128<<10 {
		t.Errorf("file grew to %d bytes", st.Size())
	}
}
