package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const hwSession = "abcd1234-0000-0000-0000-000000000000"

// The seven sections of the memory plugin's handoff skill, in the order it lists them.
var hwHeads = []string{"Goal", "Where Things Stand", "Next Action", "Dead Ends",
	"Decisions and Constraints", "Locations", "Open Questions"}

func hwKey(h string) string { return strings.ReplaceAll(strings.ToLower(h), " ", "_") }

// fakeClaude is a `claude` on PATH that logs each call (argv one per line, env, stdin) in
// dir/call.N and answers with dir/resp.N, or fails when dir/fail exists.
const fakeClaudeScript = `#!/bin/sh
n=$(ls -d "$FAKE_DIR"/call.* 2>/dev/null | wc -l | tr -d ' ') # BSD wc pads the count
c="$FAKE_DIR/call.$n"
mkdir "$c"
for a in "$@"; do printf '%s\n' "$a"; done > "$c/argv"
env > "$c/env"
cat > "$c/stdin"
if [ -e "$FAKE_DIR/fail" ]; then echo boom >&2; exit 3; fi
cat "$FAKE_DIR/resp.$n"
`

type hwEnv struct {
	t          *testing.T
	dir        string // fake claude's log dir
	cwd        string // the project
	transcript string
	next       string
	resps      int
}

func hwSetup(t *testing.T) *hwEnv {
	t.Helper()
	root := t.TempDir()
	e := &hwEnv{t: t, dir: filepath.Join(root, "log"), cwd: filepath.Join(root, "proj")}
	bin := filepath.Join(root, "bin")
	for _, d := range []string{e.dir, bin, e.cwd} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(fakeClaudeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DIR", e.dir)
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-parent-key")
	t.Setenv("XDG_RUNTIME_DIR", shortSockDir(t)) // no memory daemon unless a test starts one

	e.transcript = filepath.Join(root, "t.jsonl")
	lines := []map[string]any{
		{"type": "user", "timestamp": "2026-10-27T10:00:00Z", "cwd": "/home/dev/proj", "origin": map[string]any{"kind": "human"},
			"message": map[string]any{"role": "user", "content": "fix the widget, see @/home/dev/proj/NOTES.md; api_key=sk-ant-abcdefghijkl"}},
		{"type": "assistant", "timestamp": "2026-10-27T10:00:01Z", "message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "q1", "name": "AskUserQuestion", "input": map[string]any{"questions": []any{map[string]any{"question": "Bill where?"}}}}}}},
		{"type": "user", "timestamp": "2026-10-27T10:00:02Z", "message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "q1", "content": `"Bill where?"="Login"`}}}},
		{"type": "assistant", "timestamp": "2026-10-27T10:00:03Z", "message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "Billing goes to the login."}}}},
	}
	var b strings.Builder
	for _, l := range lines {
		j, _ := json.Marshal(l)
		b.Write(j)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(e.transcript, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	e.next = filepath.Join(e.cwd, ".handoff-next-"+hwSession+".md")
	e.writeNext("file: HANDOFF-widget.md\n## Next Steps\n1. Run make test.\n## Open Questions\n- none\n")
	e.respondStructured("")
	return e
}

func (e *hwEnv) writeNext(text string) {
	e.t.Helper()
	if err := os.WriteFile(e.next, []byte(text), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *hwEnv) respond(n int, v map[string]any) {
	e.t.Helper()
	b, _ := json.Marshal(v)
	if err := os.WriteFile(filepath.Join(e.dir, "resp."+strconv.Itoa(n)), b, 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// respondStructured queues the first call's reply: every section filled; extraFiles, when set,
// is a Files in Play-style line naming a path that is not in the transcript.
func (e *hwEnv) respondStructured(badPath string) {
	out := map[string]any{}
	for _, h := range hwHeads {
		out[hwKey(h)] = "Body of " + h + "."
	}
	out["goal"] = "> \"fix the widget\"\n\nShip the widget fix."
	out["next_action"] = "1. Run make test in ＠/home/dev/proj."
	out["decisions_and_constraints"] = "- Billing: Login, chosen by the person."
	if badPath != "" {
		out["locations"] = "- " + badPath
	}
	e.respond(0, map[string]any{"result": "", "structured_output": out, "total_cost_usd": 0.02,
		"usage": map[string]any{"input_tokens": 1000, "cache_read_input_tokens": 500, "output_tokens": 400}})
}

func (e *hwEnv) run() (path string, err error) {
	e.t.Helper()
	path, _, err = writeHandoffFile(e.transcript, e.next, e.cwd, hwSession)
	return
}

type hwCall struct {
	argv  []string
	env   map[string]string
	stdin string
}

func (e *hwEnv) calls() []hwCall {
	e.t.Helper()
	ents, _ := filepath.Glob(filepath.Join(e.dir, "call.*"))
	var out []hwCall
	for i := range ents {
		d := filepath.Join(e.dir, "call."+strconv.Itoa(i))
		rd := func(n string) string {
			b, err := os.ReadFile(filepath.Join(d, n))
			if err != nil {
				e.t.Fatal(err)
			}
			return string(b)
		}
		c := hwCall{stdin: rd("stdin"), env: map[string]string{}}
		c.argv = strings.Split(strings.TrimSuffix(rd("argv"), "\n"), "\n")
		for _, l := range strings.Split(rd("env"), "\n") {
			if k, v, ok := strings.Cut(l, "="); ok {
				c.env[k] = v
			}
		}
		out = append(out, c)
	}
	return out
}

func (c hwCall) flag(name string) (string, bool) {
	for i, a := range c.argv {
		if a == name && i+1 < len(c.argv) {
			return c.argv[i+1], true
		}
	}
	return "", false
}

func (c hwCall) has(name string) bool {
	for _, a := range c.argv {
		if a == name {
			return true
		}
	}
	return false
}

func hwRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWriteHandoffRunsOneIsolatedHaikuCall(t *testing.T) {
	e := hwSetup(t)
	if _, err := e.run(); err != nil {
		t.Fatal(err)
	}
	calls := e.calls()
	if len(calls) != 1 {
		t.Fatalf("%d claude calls, want 1", len(calls))
	}
	c := calls[0]
	for _, f := range []string{"-p", "--safe-mode", "--no-session-persistence", "--strict-mcp-config", "--json-schema"} {
		if !c.has(f) {
			t.Errorf("argv lacks %s: %q", f, c.argv)
		}
	}
	if v, ok := c.flag("--tools"); !ok || v != "" {
		t.Errorf("--tools = %q (present %v), want an empty value", v, ok)
	}
	if v, _ := c.flag("--model"); v != "claude-haiku-5-5" {
		t.Errorf("--model = %q, want claude-haiku-5-5", v)
	}
	if v, _ := c.flag("--output-format"); v != "json" {
		t.Errorf("--output-format = %q, want json", v)
	}
}

func TestWriteHandoffDropsTheAPIKeyFromTheChildEnv(t *testing.T) {
	for _, auth := range []string{"", "api", "subscription"} {
		t.Run("auth="+auth, func(t *testing.T) {
			e := hwSetup(t)
			t.Setenv("CLAUDE_TOOLS_HANDOFF_AUTH", auth) // the old switch is gone and must do nothing
			if _, err := e.run(); err != nil {
				t.Fatal(err)
			}
			for i, c := range e.calls() {
				if v, ok := c.env["ANTHROPIC_API_KEY"]; ok {
					t.Errorf("call %d: child env has ANTHROPIC_API_KEY=%q", i, v)
				}
				if _, ok := c.env["PATH"]; !ok {
					t.Errorf("call %d: the rest of the env was dropped", i)
				}
			}
		})
	}
}

func TestWriteHandoffPromptCarriesFactsNextStepsAndSkillAndHidesSecrets(t *testing.T) {
	e := hwSetup(t)
	if _, err := e.run(); err != nil {
		t.Fatal(err)
	}
	p := e.calls()[0].stdin
	if strings.Contains(p, "@") {
		t.Error("prompt still has an @, which claude -p would attach as a file")
	}
	for _, want := range []string{"＠/home/dev/proj/NOTES.md", "FACTS BLOCK START", "A: Login", "1. Run make test.", "## What the note must contain"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	for _, bad := range []string{"sk-ant-abcdefghijkl", "## How to save it", "work_state_write"} {
		if strings.Contains(p, bad) {
			t.Errorf("prompt has %q", bad)
		}
	}
}

func TestWriteHandoffRendersSevenSectionsAndUnescapesAt(t *testing.T) {
	e := hwSetup(t)
	path, err := e.run()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(e.cwd, "HANDOFF-widget.md"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	text := hwRead(t, path)
	if !strings.HasPrefix(text, "# Handoff\n") {
		t.Errorf("text starts %q", text[:20])
	}
	var heads []string
	for _, l := range strings.Split(text, "\n") {
		if h, ok := strings.CutPrefix(l, "## "); ok {
			heads = append(heads, h)
		}
	}
	if strings.Join(heads, "|") != strings.Join(hwHeads, "|") {
		t.Errorf("headings = %q, want %q", heads, hwHeads)
	}
	if !strings.Contains(text, "make test in @/home/dev/proj.") {
		t.Error("fullwidth ＠ was not turned back into @")
	}
	if head, _, _ := strings.Cut(text, "## Goal"); !strings.Contains(head, e.transcript) || !strings.Contains(head, "claude-haiku-5-5") {
		t.Errorf("header does not name the transcript and model: %q", head)
	}
}

func TestWriteHandoffNoGapsMeansNoFixCall(t *testing.T) {
	e := hwSetup(t)
	if _, err := e.run(); err != nil {
		t.Fatal(err)
	}
	if n := len(e.calls()); n != 1 {
		t.Errorf("%d calls, want 1", n)
	}
}

func TestWriteHandoffGapsTriggerExactlyOneFixCall(t *testing.T) {
	e := hwSetup(t)
	e.respondStructured("/home/nobody/nowhere/x.py")
	e.respond(1, map[string]any{"result": "# Handoff\n\n## Goal\n\nFixed.", "total_cost_usd": 0.01})
	path, err := e.run()
	if err != nil {
		t.Fatal(err)
	}
	calls := e.calls()
	if len(calls) != 2 {
		t.Fatalf("%d calls, want 2", len(calls))
	}
	if calls[1].has("--json-schema") {
		t.Error("fix call used a schema")
	}
	if v, _ := calls[1].flag("--model"); v != "claude-haiku-5-5" {
		t.Errorf("fix call model = %q", v)
	}
	if _, ok := calls[1].env["ANTHROPIC_API_KEY"]; ok {
		t.Error("fix call env has the API key")
	}
	if !strings.Contains(calls[1].stdin, "path /home/nobody/nowhere/x.py is not in the transcript") {
		t.Errorf("fix prompt does not name the gap")
	}
	if got := strings.TrimSpace(hwRead(t, path)); got != "# Handoff\n\n## Goal\n\nFixed." {
		t.Errorf("handoff = %q", got)
	}
}

func TestWriteHandoffFixReplyThatIsNotAHandoffKeepsTheDraft(t *testing.T) {
	e := hwSetup(t)
	e.respondStructured("/home/nobody/nowhere/x.py")
	e.respond(1, map[string]any{"result": "Sorry, I can't."})
	path, err := e.run()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hwRead(t, path), "/home/nobody/nowhere/x.py") {
		t.Error("the first draft was replaced")
	}
}

func TestWriteHandoffUnsafeOrMissingFileNameFallsBackToSessionName(t *testing.T) {
	for _, first := range []string{"file: ../evil.md", "## Next Steps"} {
		e := hwSetup(t)
		e.writeNext(first + "\n1. x\n")
		path, err := e.run()
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(e.cwd, "HANDOFF-abcd1234.md"); path != want {
			t.Errorf("%q: path = %q, want %q", first, path, want)
		}
	}
}

func TestWriteHandoffRemovesTheNextFileOnSuccess(t *testing.T) {
	e := hwSetup(t)
	if _, err := e.run(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.next); !os.IsNotExist(err) {
		t.Errorf("next-steps file still there (err %v)", err)
	}
}

func TestWriteHandoffClaudeFailureWritesNothingAndExitsNonzero(t *testing.T) {
	e := hwSetup(t)
	if err := os.WriteFile(filepath.Join(e.dir, "fail"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var code int
	out := captureStdout(t, func() { code = writeHandoff([]string{e.transcript, e.next, e.cwd, hwSession}) })
	if code == 0 {
		t.Error("exit code 0 after a failed claude call")
	}
	if out != "" {
		t.Errorf("stdout = %q, want nothing", out)
	}
	if m, _ := filepath.Glob(filepath.Join(e.cwd, "HANDOFF*")); len(m) != 0 {
		t.Errorf("wrote %q", m)
	}
	if _, err := os.Stat(e.next); err != nil {
		t.Errorf("next-steps file was removed: %v", err)
	}
}

func TestWriteHandoffPrintsThePathAndExitsZero(t *testing.T) {
	e := hwSetup(t)
	var code int
	out := captureStdout(t, func() { code = writeHandoff([]string{e.transcript, e.next, e.cwd, hwSession}) })
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if want := filepath.Join(e.cwd, "HANDOFF-widget.md") + "\n"; out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
}

func TestWriteHandoffUsageNeedsFourArgs(t *testing.T) {
	if code := writeHandoff([]string{"a", "b", "c"}); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
}

func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	f()
	os.Stdout = old
	w.Close()
	return <-done
}

// memoryDaemon serves POST /tool/work_state_write on $XDG_RUNTIME_DIR/memory.sock and sends
// each request body to the returned channel.
func memoryDaemon(t *testing.T, status int) <-chan map[string]any {
	t.Helper()
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(dir, "memory.sock"))
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan map[string]any, 4)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(bufio.NewReader(r.Body)).Decode(&body)
		if r.URL.Path == "/tool/work_state_write" {
			got <- body
		}
		w.WriteHeader(status)
		io.WriteString(w, "ok")
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return got
}

func TestWriteHandoffStoresABriefingInMemory(t *testing.T) {
	e := hwSetup(t)
	got := memoryDaemon(t, 200)
	path, err := e.run()
	if err != nil {
		t.Fatal(err)
	}
	body := <-got
	if body["list"] != "handoff" || body["cwd"] != e.cwd {
		t.Errorf("list/cwd = %v / %v", body["list"], body["cwd"])
	}
	fields, _ := body["fields"].(map[string]any)
	if fields["handoff_file"] != path {
		t.Errorf("handoff_file = %v, want %v", fields["handoff_file"], path)
	}
	text, _ := fields["text"].(string)
	if !strings.HasPrefix(text, "Full handoff: "+path) || !strings.Contains(text, "Ship the widget fix.") {
		t.Errorf("text = %q", text)
	}
	if !strings.Contains(text, "Run make test in @/home/dev/proj.") {
		t.Errorf("briefing lacks the Next Action section: %q", text)
	}
	if n := len([]rune(text)); n > 3000 {
		t.Errorf("briefing is %d chars", n)
	}
}

func TestWriteHandoffMemoryFailureNeverFailsTheWriter(t *testing.T) {
	t.Run("rejecting daemon", func(t *testing.T) {
		e := hwSetup(t)
		memoryDaemon(t, 400)
		path, err := e.run()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Error(err)
		}
	})
	t.Run("no daemon", func(t *testing.T) {
		e := hwSetup(t)
		if _, err := e.run(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("no XDG_RUNTIME_DIR", func(t *testing.T) {
		e := hwSetup(t)
		t.Setenv("XDG_RUNTIME_DIR", "")
		if _, err := e.run(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestWriteHandoffNothingSecretOrPrivateInOutput(t *testing.T) {
	e := hwSetup(t)
	path, err := e.run()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(hwRead(t, path), "sk-test-parent-key") {
		t.Error("the API key reached the handoff")
	}
}

// The prompt embeds the memory plugin's own handoff skill up to its save steps, which do not
// apply to a tool-less run; handoffskill.md must not drift from plugins/memory.
func TestEmbeddedSkillMatchesTheMemoryPluginSkill(t *testing.T) {
	src := hwRead(t, "../../plugins/memory/skills/handoff/SKILL.md")
	body, _, _ := strings.Cut(src, "\n## How to save it")
	_, body, ok := strings.Cut(body, "\n---\n")
	if !ok {
		t.Fatal("SKILL.md has no front matter")
	}
	if got, err := skillText(); err != nil || got != strings.TrimLeft(body, "\n") {
		t.Errorf("embedded skill differs from SKILL.md (err %v)\n got %d bytes, want %d", err, len(got), len(body))
	}
}

// capLines builds n digest lines: a user prompt, then a tool call and a tool result, repeated.
func capLines(n int) []string {
	var out []string
	for i := 0; len(out) < n; i++ {
		out = append(out, "[t] USER: prompt "+strconv.Itoa(i), "[t] TOOL Bash: command=x"+strconv.Itoa(i),
			"    -> result "+strconv.Itoa(i)+strings.Repeat("r", 80))
	}
	return out[:n]
}

func TestCapDigestUnderTheLimitIsUnchanged(t *testing.T) {
	lines := capLines(30)
	if got, want := capDigest(lines, 1<<20), strings.Join(lines, "\n"); got != want {
		t.Errorf("digest changed under the limit: %d bytes, want %d", len(got), len(want))
	}
}

func TestCapDigestDropsOldToolResultsBeforeAnythingElse(t *testing.T) {
	lines := capLines(300)
	full := len(strings.Join(lines, "\n"))
	got := capDigest(lines, full*3/4)
	if len(got) > full*3/4 {
		t.Fatalf("digest is %d bytes, limit %d", len(got), full*3/4)
	}
	if !strings.Contains(got, "omitted") {
		t.Error("no marker saying what was omitted")
	}
	for i := 0; i < 100; i++ { // every prompt and tool call survives while results are enough to drop
		for _, w := range []string{"USER: prompt " + strconv.Itoa(i) + "\n", "command=x" + strconv.Itoa(i) + "\n"} {
			if !strings.Contains(got, w) {
				t.Fatalf("non-result line %q was dropped before the results ran out", w)
			}
		}
	}
	if strings.Contains(got, "-> result 0r") {
		t.Error("oldest tool result was kept")
	}
	if !strings.HasSuffix(got, lines[len(lines)-1]) {
		t.Error("the most recent line is missing")
	}
}

func TestCapDigestThenDropsTheOldestItemsAndKeepsTheRecentEnd(t *testing.T) {
	lines := capLines(3000)
	got := capDigest(lines, 5000)
	if len(got) > 5000 {
		t.Fatalf("digest is %d bytes, limit 5000", len(got))
	}
	if !strings.HasSuffix(got, lines[len(lines)-1]) {
		t.Error("the most recent line is missing")
	}
	if strings.Contains(got, "USER: prompt 0\n") {
		t.Error("the oldest prompt was kept")
	}
	if !strings.Contains(got, "omitted") {
		t.Error("no marker saying what was omitted")
	}
	if capDigest(nil, 10) != "" {
		t.Error("empty digest is not empty")
	}
}

func TestDigestIsCappedAtDigestCap(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 400; i++ {
		j, _ := json.Marshal(map[string]any{"type": "assistant", "timestamp": "2026-10-27T10:00:03Z",
			"message": map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": strings.Repeat("x", 1900) + strconv.Itoa(i)}}}})
		b.Write(j)
		b.WriteByte('\n')
	}
	p := filepath.Join(t.TempDir(), "big.jsonl")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := digest(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > digestCap {
		t.Errorf("digest is %d bytes, cap %d", len(got), digestCap)
	}
	if !strings.Contains(got, "omitted") || !strings.HasSuffix(got, "399") {
		t.Error("digest lacks the marker or the latest item")
	}
}

func TestWriteHandoffLogsTokensAndCostPerCall(t *testing.T) {
	e := hwSetup(t)
	var buf bytes.Buffer
	logOut = &buf
	t.Cleanup(func() { logOut = os.Stderr })
	if _, err := e.run(); err != nil {
		t.Fatal(err)
	}
	if want := "writer: in=1500 out=400 $0.0200"; !strings.Contains(buf.String(), want) {
		t.Errorf("log lacks %q:\n%s", want, buf.String())
	}
}

func TestWriteHandoffLogsGapsFixCostAndRemainingGaps(t *testing.T) {
	e := hwSetup(t)
	e.respondStructured("/home/nobody/nowhere/x.py")
	e.respond(1, map[string]any{"result": "# Handoff\n\n## Locations\n\n- /home/nobody/nowhere/x.py\n\nBilling: Login", "total_cost_usd": 0.01,
		"usage": map[string]any{"input_tokens": 700, "output_tokens": 50}})
	var buf bytes.Buffer
	logOut = &buf
	t.Cleanup(func() { logOut = os.Stderr })
	if _, err := e.run(); err != nil {
		t.Fatal(err)
	}
	log := buf.String()
	for _, want := range []string{"gaps found (1):", "- path /home/nobody/nowhere/x.py is not in the transcript",
		"fix: in=700 out=50 $0.0100", "gaps remaining after fix (1):"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
}

func TestWriteHandoffLogsARejectedFixReply(t *testing.T) {
	e := hwSetup(t)
	e.respondStructured("/home/nobody/nowhere/x.py")
	e.respond(1, map[string]any{"result": "Sorry, I can't."})
	var buf bytes.Buffer
	logOut = &buf
	t.Cleanup(func() { logOut = os.Stderr })
	if _, err := e.run(); err != nil {
		t.Fatal(err)
	}
	if want := "fix reply rejected"; !strings.Contains(buf.String(), want) || !strings.Contains(buf.String(), "Sorry, I can't.") {
		t.Errorf("log does not say the fix reply was rejected:\n%s", buf.String())
	}
}

func TestWriteHandoffPromptAsksForImpersonalPhrasing(t *testing.T) {
	e := hwSetup(t)
	if _, err := e.run(); err != nil {
		t.Fatal(err)
	}
	if p := e.calls()[0].stdin; !strings.Contains(p, "third person") || !strings.Contains(p, `never "I"`) {
		t.Error("prompt does not ask for third-person phrasing without \"I\"")
	}
}

func TestFactsCapsAndDedupesBackgroundJobs(t *testing.T) {
	var b strings.Builder
	add := func(m map[string]any) {
		j, _ := json.Marshal(m)
		b.Write(j)
		b.WriteByte('\n')
	}
	for i := 0; i < 30; i++ {
		id, desc := "job"+strconv.Itoa(i), "Wait for PR "+strconv.Itoa(100+i)
		if i >= 20 { // ten distinct jobs at the end
			desc = "Distinct task " + strings.Repeat("z", i-19)
		}
		add(map[string]any{"type": "assistant", "timestamp": "2026-10-27T10:00:00Z", "message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "u" + id, "name": "Bash", "input": map[string]any{"command": "sleep 1", "run_in_background": true, "description": desc}}}}})
		add(map[string]any{"type": "user", "timestamp": "2026-10-27T10:00:01Z", "message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "u" + id, "content": "Command running in background with ID: " + id + ". Output is being written to: /tmp/" + id + ".out"}}}})
	}
	p := filepath.Join(t.TempDir(), "bg.jsonl")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	facts, err := buildFacts(p, "")
	if err != nil {
		t.Fatal(err)
	}
	section := facts[strings.Index(facts, "### Background jobs"):]
	if n := strings.Count(section, "\n- job"); n > maxBgJobs {
		t.Errorf("%d job lines, cap %d:\n%s", n, maxBgJobs, section)
	}
	if n := strings.Count(section, "Wait for PR"); n > 1 {
		t.Errorf("%d near-identical Wait for PR lines, want 1 (the latest):\n%s", n, section)
	}
	if !strings.Contains(section, "- job29:") || strings.Contains(section, "- job0:") {
		t.Errorf("did not keep the latest jobs:\n%s", section)
	}
	if !strings.Contains(section, "omitted") {
		t.Errorf("no note on how many jobs were dropped:\n%s", section)
	}
}
