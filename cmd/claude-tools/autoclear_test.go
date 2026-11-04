package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// autoclearWorld gives one test its own state dir, a fake writer in place of
// `claude-tools write-handoff`, and the background half run in-process. The fake follows the
// writer's contract: <transcript> <next-steps file> <cwd> <session>; it writes HANDOFF-w.md
// in cwd, removes the next-steps file and prints the path, or fails when FAKE_WRITER_FAIL is set.
type autoclearWorld struct {
	state string
	cwd   string
}

const fakeWriter = `
if [ -n "$FAKE_WRITER_FAIL" ]; then echo "Traceback: boom" >&2; exit 1; fi
printf '# Handoff\n\nfrom %s\n' "$1" > "$3/HANDOFF-w.md"
rm -f "$2"
echo "$4 written 1s" >&2
echo "$3/HANDOFF-w.md"
`

func newAutoclearWorld(t *testing.T) *autoclearWorld {
	t.Helper()
	base := t.TempDir()
	t.Setenv("HANDOFF_STATE_DIR", base)
	t.Setenv("HANDOFF_AUTOCLEAR", "")
	t.Setenv("FAKE_WRITER_FAIL", "")
	oldWriter, oldBg := writerCommand, startBackground
	t.Cleanup(func() { writerCommand, startBackground = oldWriter, oldBg })
	writerCommand = func(transcript, next, cwd, session string) (*exec.Cmd, error) {
		return exec.Command("sh", "-c", fakeWriter, "writer", transcript, next, cwd, session), nil
	}
	startBackground = func(args ...string) { autoclear(args) }
	w := &autoclearWorld{state: filepath.Join(base, "claude-handoff")}
	if err := os.MkdirAll(w.state, 0o755); err != nil {
		t.Fatal(err)
	}
	return w
}

// setup makes a fresh cwd with a transcript and a gate mark a minute old.
func (w *autoclearWorld) setup(t *testing.T, session string) {
	t.Helper()
	w.cwd = filepath.Join(t.TempDir(), "cwd-"+session)
	if err := os.MkdirAll(w.cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(w.transcript(), []byte("{}\n"), 0o644)
	minuteAgo := time.Now().Add(-time.Minute)
	mark := filepath.Join(w.state, session+".gate")
	os.WriteFile(mark, nil, 0o644)
	os.Chtimes(mark, minuteAgo, minuteAgo)
}

func (w *autoclearWorld) transcript() string { return filepath.Join(w.cwd, "prev.jsonl") }

func (w *autoclearWorld) nextFile(session string) string {
	return filepath.Join(w.cwd, ".handoff-next-"+session+".md")
}

func (w *autoclearWorld) nextSteps(t *testing.T, session string) {
	t.Helper()
	body := "file: HANDOFF-w.md\n## Next Steps\n1. go\n"
	if err := os.WriteFile(w.nextFile(session), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (w *autoclearWorld) handoff() string { return filepath.Join(w.cwd, "HANDOFF-w.md") }

// stop runs the Stop hook for session; active is stop_hook_active.
func (w *autoclearWorld) stop(t *testing.T, session string, active bool) (int, string) {
	t.Helper()
	a := "false"
	if active {
		a = "true"
	}
	in := `{"session_id":"` + session + `","transcript_path":"` + w.transcript() + `","cwd":"` + w.cwd +
		`","hook_event_name":"Stop","stop_hook_active":` + a + `}`
	code, out, _ := runHook(t, autoclear, in)
	return code, out
}

// cli runs a context-guard mode (--claim, --done).
func (w *autoclearWorld) cli(t *testing.T, args ...string) (int, string) {
	t.Helper()
	code, out, _ := runHook(t, func([]string) int { return autoclear(args) }, "")
	return code, out
}

// armed is the arm file's lines joined by ';', or "none".
func (w *autoclearWorld) armed(session string) string {
	b, err := os.ReadFile(filepath.Join(w.state, session+".rearm"))
	if err != nil {
		return "none"
	}
	return strings.ReplaceAll(string(b), "\n", ";")
}

func (w *autoclearWorld) logged(t *testing.T, want string) int {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(w.state, "writer.log"))
	return strings.Count(string(b), want)
}

func (w *autoclearWorld) resume() string {
	return "Continue from the handoff in " + w.handoff() + ". If it lacks something you need, search the previous session's transcript (" +
		w.transcript() + ", JSON Lines) with rg or jq before asking."
}

// armedOnce runs a writer-path stop and leaves the clear armed for session.
func (w *autoclearWorld) armedOnce(t *testing.T, session string) {
	t.Helper()
	w.setup(t, session)
	w.nextSteps(t, session)
	w.stop(t, session, true)
	if w.armed(session) == "none" {
		t.Fatalf("setup: %s not armed", session)
	}
}

func TestAutoclearWritesThenArms(t *testing.T) {
	w := newAutoclearWorld(t)
	w.setup(t, "w1")
	w.nextSteps(t, "w1")
	code, out := w.stop(t, "w1", true)
	if code != 0 || out != "" {
		t.Fatalf("exit %d, stdout %q; the hook must never block the stop", code, out)
	}
	if got, want := w.armed("w1"), w.handoff()+";"+w.transcript()+";"; got != want {
		t.Errorf("armed %q, want %q", got, want)
	}
	if b, _ := os.ReadFile(w.handoff()); !strings.Contains(string(b), "from "+w.transcript()) {
		t.Errorf("writer did not get the transcript: %q", b)
	}
	if n := w.logged(t, "w1 written"); n != 1 {
		t.Errorf("writer summary logged %d times, want 1", n)
	}
	if n := w.logged(t, "w1 writer start"); n != 1 {
		t.Errorf("writer start logged %d times, want 1", n)
	}
	if n := w.logged(t, "w1 clear armed: "+w.handoff()); n != 1 {
		t.Errorf("arming logged %d times, want 1", n)
	}
}

// The state dir is the one gate uses: $HANDOFF_STATE_DIR/claude-handoff, never /tmp.
func TestAutoclearUsesGateStateDir(t *testing.T) {
	w := newAutoclearWorld(t)
	w.armedOnce(t, "sd")
	if !exists(filepath.Join(os.Getenv("HANDOFF_STATE_DIR"), "claude-handoff", "sd.rearm")) {
		t.Error("arm file is not in the gate's state dir")
	}
	if !exists(filepath.Join(w.state, "sd.autoclear")) {
		t.Error("once-mark is not in the gate's state dir")
	}
}

func TestAutoclearOncePerSession(t *testing.T) {
	w := newAutoclearWorld(t)
	w.armedOnce(t, "w7")
	os.Remove(w.handoff())
	os.Remove(filepath.Join(w.state, "w7.rearm"))
	w.nextSteps(t, "w7")
	w.stop(t, "w7", true)
	if exists(w.handoff()) {
		t.Error("a second stop ran the writer again")
	}
	if got := w.armed("w7"); got != "none" {
		t.Errorf("a second stop armed %q", got)
	}
}

func TestAutoclearStaysQuiet(t *testing.T) {
	tests := []struct {
		name  string
		setup func(w *autoclearWorld, t *testing.T) (session string, active bool)
	}{
		{"no gate mark", func(w *autoclearWorld, t *testing.T) (string, bool) {
			w.setup(t, "q1")
			w.nextSteps(t, "q1")
			os.Remove(filepath.Join(w.state, "q1.gate"))
			return "q1", true
		}},
		{"next-steps file older than the gate", func(w *autoclearWorld, t *testing.T) (string, bool) {
			w.setup(t, "q2")
			w.nextSteps(t, "q2")
			old := time.Now().Add(-time.Hour)
			os.Chtimes(w.nextFile("q2"), old, old)
			return "q2", true
		}},
		{"the stop that is not after the gate's block", func(w *autoclearWorld, t *testing.T) (string, bool) {
			w.setup(t, "q3")
			w.nextSteps(t, "q3")
			return "q3", false
		}},
		{"no next-steps file", func(w *autoclearWorld, t *testing.T) (string, bool) {
			w.setup(t, "q4")
			return "q4", true
		}},
		{"a HANDOFF.md written without next steps (the dropped legacy path)", func(w *autoclearWorld, t *testing.T) (string, bool) {
			w.setup(t, "q5")
			os.WriteFile(filepath.Join(w.cwd, "HANDOFF.md"), []byte("# Handoff\n\nstuff\n"), 0o644)
			return "q5", true
		}},
		{"cwd is gone", func(w *autoclearWorld, t *testing.T) (string, bool) {
			w.setup(t, "q6")
			w.nextSteps(t, "q6")
			w.cwd = filepath.Join(w.cwd, "missing")
			return "q6", true
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newAutoclearWorld(t)
			session, active := tt.setup(w, t)
			code, out := w.stop(t, session, active)
			if code != 0 || out != "" {
				t.Errorf("exit %d, stdout %q", code, out)
			}
			if got := w.armed(session); got != "none" {
				t.Errorf("armed %q, want nothing", got)
			}
			if exists(filepath.Join(w.state, session+".autoclear")) {
				t.Error("the once-mark was used up")
			}
		})
	}
}

// The stop that is not after the gate's block must not use up the one shot.
func TestAutoclearInactiveStopKeepsTheOneShot(t *testing.T) {
	w := newAutoclearWorld(t)
	w.setup(t, "o1")
	w.nextSteps(t, "o1")
	w.stop(t, "o1", false)
	w.stop(t, "o1", true)
	if w.armed("o1") == "none" {
		t.Error("the active stop after an inactive one did not arm")
	}
}

func TestAutoclearUnsafeSessionIDs(t *testing.T) {
	w := newAutoclearWorld(t)
	w.setup(t, "u1")
	w.nextSteps(t, "u1")
	w.stop(t, "../evil", true)
	entries, _ := os.ReadDir(w.state)
	for _, e := range entries {
		if strings.Contains(e.Name(), "evil") {
			t.Errorf("unsafe id left %s in the state dir", e.Name())
		}
	}
	if code, _ := w.cli(t, "--claim", "../evil"); code != 1 {
		t.Errorf("--claim with an unsafe id exited %d, want 1", code)
	}
	if code, _ := w.cli(t, "--done", "../evil", "ok", "x"); code != 1 {
		t.Errorf("--done with an unsafe id exited %d, want 1", code)
	}
}

// HANDOFF_AUTOCLEAR=0 still writes the handoff; it only skips arming the clear.
func TestAutoclearOptOutStillWritesTheHandoff(t *testing.T) {
	w := newAutoclearWorld(t)
	t.Setenv("HANDOFF_AUTOCLEAR", "0")
	w.setup(t, "x1")
	w.nextSteps(t, "x1")
	w.stop(t, "x1", true)
	if b, _ := os.ReadFile(w.handoff()); !strings.HasPrefix(string(b), "# Handoff") {
		t.Errorf("handoff not written: %q", b)
	}
	if got := w.armed("x1"); got != "none" {
		t.Errorf("armed %q, want nothing", got)
	}
	if n := w.logged(t, "x1 no clear: HANDOFF_AUTOCLEAR=0"); n != 1 {
		t.Errorf("opt-out logged %d times, want 1", n)
	}
}

func TestAutoclearWriterFailure(t *testing.T) {
	w := newAutoclearWorld(t)
	t.Setenv("FAKE_WRITER_FAIL", "1")
	w.setup(t, "f1")
	w.nextSteps(t, "f1")
	w.stop(t, "f1", true)
	if got := w.armed("f1"); got != "none" {
		t.Errorf("armed %q after a writer failure", got)
	}
	if n := w.logged(t, "f1 writer failed"); n != 1 {
		t.Errorf("failure logged %d times, want 1", n)
	}
	if n := w.logged(t, "Traceback: boom"); n != 1 {
		t.Errorf("the writer's stderr reached writer.log %d times, want 1", n)
	}
}

// The writer printing no path is a failure, not a clear into an empty file name.
func TestAutoclearWriterWithNoOutput(t *testing.T) {
	w := newAutoclearWorld(t)
	writerCommand = func(transcript, next, cwd, session string) (*exec.Cmd, error) {
		return exec.Command("true"), nil
	}
	w.setup(t, "e1")
	w.nextSteps(t, "e1")
	w.stop(t, "e1", true)
	if got := w.armed("e1"); got != "none" {
		t.Errorf("armed %q after a writer that printed no path", got)
	}
	if n := w.logged(t, "e1 writer failed"); n != 1 {
		t.Errorf("failure logged %d times, want 1", n)
	}
}

// The default writer is this binary's own write-handoff subcommand, with the writer's
// four arguments.
func TestDefaultWriterIsWriteHandoff(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	cmd, err := defaultWriterCommand("/t.jsonl", "/n.md", "/w", "sess")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{self, "write-handoff", "/t.jsonl", "/n.md", "/w", "sess"}
	if strings.Join(cmd.Args, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("args %q, want %q", cmd.Args, want)
	}
}

func TestAutoclearClaim(t *testing.T) {
	w := newAutoclearWorld(t)
	w.armedOnce(t, "c1")
	code, out := w.cli(t, "--claim", "c1")
	if code != 0 || out != w.resume()+"\n" {
		t.Errorf("--claim: exit %d, stdout %q; want 0 and the continue prompt %q", code, out, w.resume())
	}
	if got := w.armed("c1"); got != "none" {
		t.Errorf("--claim left the arm file: %q", got)
	}
	if code, out := w.cli(t, "--claim", "c1"); code != 1 || out != "" {
		t.Errorf("second --claim: exit %d, stdout %q; want 1 and nothing", code, out)
	}
	if code, out := w.cli(t, "--claim", "never-armed"); code != 1 || out != "" {
		t.Errorf("--claim with nothing armed: exit %d, stdout %q", code, out)
	}
}

func TestAutoclearClaimWithoutTranscript(t *testing.T) {
	w := newAutoclearWorld(t)
	w.armedOnce(t, "c2")
	os.Remove(w.transcript())
	_, out := w.cli(t, "--claim", "c2")
	if want := "Continue from the handoff in " + w.handoff() + ".\n"; out != want {
		t.Errorf("stdout %q, want %q", out, want)
	}
}

func TestAutoclearClaimAfterTheHandoffIsGone(t *testing.T) {
	w := newAutoclearWorld(t)
	w.armedOnce(t, "c3")
	os.Remove(w.handoff())
	if code, out := w.cli(t, "--claim", "c3"); code != 1 || out != "" {
		t.Errorf("exit %d, stdout %q; want 1 and nothing", code, out)
	}
}

func TestAutoclearDone(t *testing.T) {
	w := newAutoclearWorld(t)
	w.armedOnce(t, "d1")
	w.cli(t, "--claim", "d1")
	if code, _ := w.cli(t, "--done", "d1", "ok", "cleared by context-guard into d1-new"); code != 0 {
		t.Errorf("--done ok exited %d", code)
	}
	if n := w.logged(t, "d1 cleared by context-guard into d1-new"); n != 1 {
		t.Errorf("--done ok logged %d times, want 1", n)
	}
	if got := w.armed("d1"); got != "none" {
		t.Errorf("--done ok left %q armed", got)
	}
	if code, _ := w.cli(t, "--claim", "d1"); code != 1 {
		t.Errorf("--claim after --done ok exited %d, want 1", code)
	}
	if code, _ := w.cli(t, "--done", "d1", "ok", "again"); code != 1 {
		t.Errorf("--done without a claim exited %d, want 1", code)
	}
}

func TestAutoclearDoneFailRearms(t *testing.T) {
	w := newAutoclearWorld(t)
	w.armedOnce(t, "d2")
	w.cli(t, "--claim", "d2")
	if code, _ := w.cli(t, "--done", "d2", "fail", "no clear: boom"); code != 0 {
		t.Errorf("--done fail exited %d", code)
	}
	if n := w.logged(t, "d2 no clear: boom (re-armed)"); n != 1 {
		t.Errorf("--done fail logged %d times, want 1", n)
	}
	if got, want := w.armed("d2"), w.handoff()+";"+w.transcript()+";"; got != want {
		t.Errorf("re-armed %q, want %q", got, want)
	}
	if _, out := w.cli(t, "--claim", "d2"); out != w.resume()+"\n" {
		t.Errorf("claim after re-arm: %q", out)
	}
}

func TestAutoclearDoneUnknownOutcome(t *testing.T) {
	w := newAutoclearWorld(t)
	w.armedOnce(t, "d3")
	w.cli(t, "--claim", "d3")
	if code, _ := w.cli(t, "--done", "d3", "maybe", "x"); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
}

// A bad invocation of the Stop hook (not JSON) is silent.
func TestAutoclearNotJSON(t *testing.T) {
	newAutoclearWorld(t)
	code, out, _ := runHook(t, autoclear, "not json")
	if code != 0 || out != "" {
		t.Errorf("exit %d, stdout %q", code, out)
	}
}
