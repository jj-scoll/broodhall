package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Progress markers the statusline reads: <session>.writing (start epoch) while the writer
// runs, then .rearm (armed), .written (autoclear off) or .failed; .cleared after --done ok.

func (w *autoclearWorld) mark(session, suffix string) string {
	b, err := os.ReadFile(filepath.Join(w.state, session+"."+suffix))
	if err != nil {
		return "none"
	}
	return strings.TrimRight(string(b), "\n")
}

func TestAutoclearWritingMarkerDuringWriter(t *testing.T) {
	w := newAutoclearWorld(t)
	w.setup(t, "m1")
	w.nextSteps(t, "m1")
	seen := filepath.Join(t.TempDir(), "seen")
	writerCommand = func(transcript, next, cwd, session string) (*exec.Cmd, error) {
		script := `cat "` + filepath.Join(w.state, "m1.writing") + `" > "` + seen + `"` + "\n" + fakeWriter
		return exec.Command("sh", "-c", script, "writer", transcript, next, cwd, session), nil
	}
	before := time.Now().Unix()
	w.stop(t, "m1", true)
	got, err := os.ReadFile(seen)
	if err != nil {
		t.Fatalf("no .writing marker while the writer ran: %v", err)
	}
	epoch, err := strconv.ParseInt(strings.TrimSpace(string(got)), 10, 64)
	if err != nil || epoch < before-1 || epoch > time.Now().Unix()+1 {
		t.Errorf(".writing holds %q, want the start epoch", got)
	}
	if exists(filepath.Join(w.state, "m1.writing")) {
		t.Error(".writing left behind after the writer exited")
	}
	if w.armed("m1") == "none" {
		t.Error("clear not armed after a successful write")
	}
	if exists(filepath.Join(w.state, "m1.failed")) || exists(filepath.Join(w.state, "m1.written")) {
		t.Error("failure or autoclear-off marker on a successful, armed write")
	}
}

func TestAutoclearFailedMarker(t *testing.T) {
	w := newAutoclearWorld(t)
	t.Setenv("FAKE_WRITER_FAIL", "1")
	w.setup(t, "m2")
	w.nextSteps(t, "m2")
	w.stop(t, "m2", true)
	if !exists(filepath.Join(w.state, "m2.failed")) {
		t.Error("no .failed marker after the writer failed")
	}
	if exists(filepath.Join(w.state, "m2.writing")) {
		t.Error(".writing left behind after the writer failed")
	}
}

func TestAutoclearWrittenMarkerWhenAutoclearOff(t *testing.T) {
	w := newAutoclearWorld(t)
	t.Setenv("HANDOFF_AUTOCLEAR", "0")
	w.setup(t, "m3")
	w.nextSteps(t, "m3")
	w.stop(t, "m3", true)
	if got := w.mark("m3", "written"); got != w.handoff() {
		t.Errorf(".written = %q, want %q", got, w.handoff())
	}
	if exists(filepath.Join(w.state, "m3.writing")) || w.armed("m3") != "none" {
		t.Error("writing marker left, or a clear armed, with HANDOFF_AUTOCLEAR=0")
	}
}

func TestAutoclearClearedMarkerOnDoneOK(t *testing.T) {
	w := newAutoclearWorld(t)
	w.armedOnce(t, "m4")
	if code, _ := w.cli(t, "--claim", "m4"); code != 0 {
		t.Fatal("claim failed")
	}
	if code, _ := w.cli(t, "--done", "m4", "ok", "cleared"); code != 0 {
		t.Fatal("done failed")
	}
	if exists(filepath.Join(w.state, "m4.claimed")) {
		t.Error(".claimed left after --done ok")
	}
	if first, _, _ := strings.Cut(w.mark("m4", "cleared"), "\n"); first != w.handoff() {
		t.Errorf(".cleared first line = %q, want %q", first, w.handoff())
	}
}

func TestAutoclearNewWriteClearsOldOutcomeMarkers(t *testing.T) {
	w := newAutoclearWorld(t)
	w.setup(t, "m5")
	for _, s := range []string{"failed", "written", "cleared"} {
		os.WriteFile(filepath.Join(w.state, "m5."+s), []byte("old\n"), 0o644)
	}
	w.nextSteps(t, "m5")
	w.stop(t, "m5", true)
	for _, s := range []string{"failed", "written", "cleared"} {
		if exists(filepath.Join(w.state, "m5."+s)) {
			t.Errorf("stale .%s survived a new writer run", s)
		}
	}
}
