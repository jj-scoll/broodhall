package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// runHook runs a hook subcommand in-process with stdin set to input and returns its exit
// code, stdout and stderr, the way Claude Code sees them.
func runHook(t *testing.T, fn func([]string) int, input string) (code int, stdout, stderr string) {
	t.Helper()
	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = inR, outW, errW
	defer func() { os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr }()
	go func() { io.WriteString(inW, input); inW.Close() }()
	code = fn(nil)
	outW.Close()
	errW.Close()
	o, _ := io.ReadAll(outR)
	e, _ := io.ReadAll(errR)
	return code, string(o), string(e)
}

type hookReply struct {
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

// lookupFixture makes a project dir with a big file and stubs memdLookup/projectKey. calls
// counts memdLookup invocations; lastPath/lastProject record the arguments.
type lookupFixture struct {
	dir, file   string
	calls       atomic.Int32
	lastPath    atomic.Value
	lastProject atomic.Value
	reply       string
}

func newLookupFixture(t *testing.T, reply string) *lookupFixture {
	t.Helper()
	f := &lookupFixture{dir: t.TempDir(), reply: reply}
	f.file = filepath.Join(f.dir, "big.go")
	if err := os.WriteFile(f.file, []byte(strings.Repeat("x", 2000)), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	t.Setenv("MEMORY_LOOKUP_SHELL", "")
	oldL, oldP := memdLookup, memdProjectKey
	memdLookup = func(path, project string) string {
		f.calls.Add(1)
		f.lastPath.Store(path)
		f.lastProject.Store(project)
		return f.reply
	}
	memdProjectKey = func(cwd string) string { return "key:" + cwd }
	t.Cleanup(func() { memdLookup, memdProjectKey = oldL, oldP })
	return f
}

func (f *lookupFixture) payload(tool, inputJSON, extra string) string {
	return `{"session_id":"s","cwd":"` + f.dir + `","tool_name":"` + tool + `","tool_input":` + inputJSON + extra + `}`
}

func runLookup(t *testing.T, payload string) (int, string) {
	t.Helper()
	var o bytes.Buffer
	c := memLookup(strings.NewReader(payload), &o)
	return c, o.String()
}

func TestLookupInjectsForEditWriteNotebook(t *testing.T) {
	f := newLookupFixture(t, "#1 earlier work on big.go")
	for _, tool := range []string{"Edit", "Write", "NotebookEdit"} {
		in := `{"file_path":"` + f.file + `"}`
		if tool == "NotebookEdit" {
			in = `{"notebook_path":"` + f.file + `"}`
		}
		code, out := runLookup(t, f.payload(tool, in, ""))
		var r hookReply
		if code != 0 || !strings.Contains(out, `"hookEventName": "PreToolUse"`) {
			t.Fatalf("%s: code %d out %q", tool, code, out)
		}
		if err := json.Unmarshal([]byte(out), &r); err != nil || r.HookSpecificOutput.AdditionalContext != "#1 earlier work on big.go" {
			t.Fatalf("%s: reply %q (%v)", tool, out, err)
		}
	}
}

func TestLookupIgnoresReadAndBadInput(t *testing.T) {
	f := newLookupFixture(t, "text")
	for _, in := range []string{f.payload("Read", `{"file_path":"`+f.file+`"}`, ""), `not json`, ``} {
		if code, out := runLookup(t, in); code != 0 || out != "" {
			t.Errorf("%q: code %d out %q, want silent", in, code, out)
		}
	}
	if n := f.calls.Load(); n != 0 {
		t.Fatalf("lookup called %d times", n)
	}
}

func TestMemdLookupHangingSocketRespectsBudget(t *testing.T) {
	dir := shortSockDir(t)
	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	os.Rename(sock, filepath.Join(dir, "memory.sock"))
	start := time.Now()
	got := memdLookup("/x/y.go", "p")
	el := time.Since(start)
	t.Logf("hanging socket lookup took %v", el)
	if got != "" || el > memLookupBudget+50*time.Millisecond {
		t.Fatalf("got %q in %v", got, el)
	}
}

func TestMemdLookupMissingSocketIsFast(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	start := time.Now()
	got := memdLookup("/x/y.go", "p")
	el := time.Since(start)
	t.Logf("missing socket lookup took %v", el)
	if got != "" || el > 20*time.Millisecond {
		t.Fatalf("got %q in %v", got, el)
	}
}

func TestMemdLookupTextCappedAt1500(t *testing.T) {
	f := newLookupFixture(t, strings.Repeat("a", 3000))
	_, out := runLookup(t, f.payload("Edit", `{"file_path":"`+f.file+`"}`, ""))
	var r hookReply
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	if n := len(r.HookSpecificOutput.AdditionalContext); n != 1500 {
		t.Fatalf("context is %d bytes, want 1500", n)
	}
}

func TestMemdLookupSkipRules(t *testing.T) {
	f := newLookupFixture(t, "text")
	small := filepath.Join(f.dir, "small.go")
	os.WriteFile(small, []byte("tiny"), 0o644)
	cases := []struct {
		name, file, extra string
	}{
		{"agent_id", f.file, `,"agent_id":"a1"`},
		{"agentId", f.file, `,"agentId":"a1"`},
		{"under 1500 bytes", small, ""},
		{"missing", filepath.Join(f.dir, "nope.go"), ""},
		{"directory", f.dir, ""},
	}
	for _, c := range cases {
		_, out := runLookup(t, f.payload("Edit", `{"file_path":"`+c.file+`"}`, c.extra))
		if out != "" {
			t.Errorf("%s: got output %q", c.name, out)
		}
	}
	if n := f.calls.Load(); n != 0 {
		t.Fatalf("lookup called %d times", n)
	}
	// Relative path resolves against cwd.
	_, out := runLookup(t, f.payload("Edit", `{"file_path":"big.go"}`, ""))
	if !strings.Contains(out, "text") || f.calls.Load() != 1 {
		t.Fatalf("relative path: out %q calls %d", out, f.calls.Load())
	}
	if p, _ := f.lastPath.Load().(string); p != f.file {
		t.Fatalf("lookup path %q, want %q", p, f.file)
	}
}

func TestMemdLookupShellSwitch(t *testing.T) {
	f := newLookupFixture(t, "shell ctx")
	bash := f.payload("Bash", `{"command":"cat big.go | head -5"}`, "")
	run := func(p string) string { _, out := runLookup(t, p); return out }
	if out := run(bash); out != "" || f.calls.Load() != 0 {
		t.Fatalf("switch off: out %q calls %d", out, f.calls.Load())
	}
	grep := f.payload("Grep", `{"pattern":"x","path":"`+f.file+`"}`, "")
	if out := run(grep); out != "" || f.calls.Load() != 0 {
		t.Fatalf("switch off grep: out %q calls %d", out, f.calls.Load())
	}
	t.Setenv("MEMORY_LOOKUP_SHELL", "1")
	if out := run(bash); !strings.Contains(out, "shell ctx") || f.calls.Load() != 1 {
		t.Fatalf("switch on bash: out %q calls %d", out, f.calls.Load())
	}
	if out := run(grep); !strings.Contains(out, "shell ctx") || f.calls.Load() != 2 {
		t.Fatalf("switch on grep: out %q calls %d", out, f.calls.Load())
	}
	// A command that names no existing file does no lookup.
	if out := run(f.payload("Bash", `{"command":"ls -la"}`, "")); out != "" || f.calls.Load() != 2 {
		t.Fatalf("no file: out %q calls %d", out, f.calls.Load())
	}
}

func TestMemdLookupUsesClaudeProjectDir(t *testing.T) {
	f := newLookupFixture(t, "ctx")
	other := t.TempDir()
	t.Setenv("CLAUDE_PROJECT_DIR", other)
	runLookup(t, f.payload("Edit", `{"file_path":"`+f.file+`"}`, ""))
	if p, _ := f.lastProject.Load().(string); p != "key:"+other {
		t.Fatalf("project %q, want key of CLAUDE_PROJECT_DIR", p)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	runLookup(t, f.payload("Edit", `{"file_path":"`+f.file+`"}`, ""))
	if p, _ := f.lastProject.Load().(string); p != "key:"+f.dir {
		t.Fatalf("project %q, want key of cwd", p)
	}
}

func TestMemdLookupColdProjectKeyRespectsBudget(t *testing.T) {
	f := newLookupFixture(t, "ctx")
	memdProjectKey = func(string) string { time.Sleep(2 * time.Second); return "slow" }
	start := time.Now()
	_, out := runLookup(t, f.payload("Edit", `{"file_path":"`+f.file+`"}`, ""))
	el := time.Since(start)
	t.Logf("cold projectKey lookup took %v", el)
	if out != "" || el > memLookupBudget+50*time.Millisecond {
		t.Fatalf("out %q in %v", out, el)
	}
}

// fakeMemd serves canned replies on a temp unix socket.
func fakeMemd(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	dir := shortSockDir(t)
	l, err := net.Listen("unix", filepath.Join(dir, "memory.sock"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	t.Setenv("XDG_RUNTIME_DIR", dir)
}

func TestMemGet(t *testing.T) {
	fakeMemd(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Write([]byte("hello " + r.URL.Query().Get("q")))
		default:
			http.Error(w, "boom", 500)
		}
	})
	b, err := memGet(memdSockPath(), "/ok?q=a%20b", time.Second)
	if err != nil || string(b) != "hello a b" {
		t.Fatalf("%q %v", b, err)
	}
	if _, err := memGet(memdSockPath(), "/bad", time.Second); err == nil {
		t.Fatal("non-200 must be an error")
	}
	if _, err := memGet(memdSockPath()+"x", "/ok", time.Second); err == nil {
		t.Fatal("missing socket must be an error")
	}
}

func TestMemdLookupRealClient(t *testing.T) {
	fakeMemd(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("path=" + r.URL.Query().Get("path") + " project=" + r.URL.Query().Get("project")))
	})
	if got := memdLookup("/a b/c.go", "my/proj"); got != "path=/a b/c.go project=my/proj" {
		t.Fatalf("%q", got)
	}
}

func TestMemContext(t *testing.T) {
	const want = `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"CTX"}}`
	var gotProject atomic.Value
	fakeMemd(t, func(w http.ResponseWriter, r *http.Request) {
		gotProject.Store(r.URL.Query().Get("project"))
		w.Write([]byte(want))
	})
	oldP := memdProjectKey
	memdProjectKey = func(cwd string) string { return "key:" + cwd }
	t.Cleanup(func() { memdProjectKey = oldP })
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	code, out, _ := runHook(t, memContext, `{"cwd":"/w/p"}`)
	if code != 0 || out != want || gotProject.Load() != "key:/w/p" {
		t.Fatalf("%d %q project %v", code, out, gotProject.Load())
	}
	t.Setenv("CLAUDE_PROJECT_DIR", "/env/dir")
	runHook(t, memContext, `{"cwd":"/w/p"}`)
	if gotProject.Load() != "key:/env/dir" {
		t.Fatalf("project %v, want key of CLAUDE_PROJECT_DIR", gotProject.Load())
	}
}

func TestMemContextErrorsPrintEmptyBlock(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir()) // no daemon
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	for _, in := range []string{`{"cwd":"/w/p"}`, `not json`, ``} {
		code, out, _ := runHook(t, memContext, in)
		if code != 0 || !strings.Contains(out, `"hookEventName":"SessionStart"`) || !strings.Contains(out, `"additionalContext":""`) {
			t.Errorf("%q: %d %q", in, code, out)
		}
	}
}

func TestRunDispatch(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	var e bytes.Buffer
	if got := run(nil, &e); got != 2 || !strings.Contains(e.String(), "usage: memory") {
		t.Errorf("no args: %d %q", got, e.String())
	}
	e.Reset()
	if got := run([]string{"nope"}, &e); got != 2 || !strings.Contains(e.String(), `unknown subcommand "nope"`) {
		t.Errorf("unknown: %d %q", got, e.String())
	}
	code, out, _ := runHook(t, func([]string) int { return run([]string{"context"}, io.Discard) }, `{"cwd":"/w/p"}`)
	if code != 0 || !strings.Contains(out, "SessionStart") {
		t.Fatalf("context via run: %d %q", code, out)
	}
	code, out, _ = runHook(t, func([]string) int { return run([]string{"lookup"}, io.Discard) }, `not json`)
	if code != 0 || out != "" {
		t.Fatalf("lookup via run: %d %q", code, out)
	}
}
