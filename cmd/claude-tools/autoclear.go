package main

// autoclear (Stop hook): after gate fired and the session wrote .handoff-next-<session>.md,
// get the handoff written, then arm the clear: <state>/<session>.rearm holds the handoff file
// and the old transcript. The context-guard plugin watches for it, takes it with --claim,
// runs /clear and submits "Continue from the handoff in <file>." in the fresh session (or
// carries a prompt typed meanwhile into it), then reports with --done.
//
// The handoff is written by `claude-tools write-handoff` in the background (about 1.5
// minutes); its stderr summary or traceback goes to <state>/writer.log. Never blocks the
// stop. Once per handoff. Every outcome is logged to writer.log. Opt out of the clear with
// HANDOFF_AUTOCLEAR=0 (the handoff is still written).
//
// Modes for context-guard:
//   --claim <session>: take the armed clear; print its continue prompt, else exit 1.
//   --done <session> ok|fail <text>: log the claimed clear's outcome; fail re-arms it.
// Background half, started by the hook itself:
//   --write <session> <next-steps file> <cwd> <transcript>

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// writerCommand builds the handoff writer's command; tests swap in a fake.
var writerCommand = defaultWriterCommand

// startBackground runs a background half of autoclear detached from the hook; tests run it
// in-process.
var startBackground = detach

func defaultWriterCommand(transcript, next, cwd, session string) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return exec.Command(self, "write-handoff", transcript, next, cwd, session), nil
}

func detach(args ...string) {
	self, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(self, append([]string{"autoclear"}, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() == nil {
		cmd.Process.Release()
	}
}

func readStopInput() (stopInput, bool) {
	var in stopInput
	data, err := io.ReadAll(os.Stdin)
	if err != nil || json.Unmarshal(data, &in) != nil {
		return in, false
	}
	return in, true
}

// continueLine is the prompt submitted in the new session.
func continueLine(file, transcript string) string {
	// The handoff misses things; the old transcript (still on disk) has them.
	hint := ""
	if isFile(transcript) {
		hint = " If it lacks something you need, search the previous session's transcript (" + transcript + ", JSON Lines) with rg or jq before asking."
	}
	return "Continue from the handoff in " + file + "." + hint
}

// arm leaves <session>.rearm (handoff file, transcript) for context-guard to claim, unless
// the clear is opted out.
func arm(state, session, file, transcript string) {
	if os.Getenv("HANDOFF_AUTOCLEAR") == "0" {
		logWriter(state, session+" no clear: HANDOFF_AUTOCLEAR=0")
		os.WriteFile(filepath.Join(state, session+".written"), []byte(file+"\n"), 0o644) // for the statusline
		return
	}
	if err := os.WriteFile(filepath.Join(state, session+".rearm"), []byte(file+"\n"+transcript+"\n"), 0o644); err != nil {
		logWriter(state, session+" no clear: "+err.Error())
		return
	}
	logWriter(state, session+" clear armed: "+file)
}

// claim moves <session>.rearm to <session>.claimed and returns its handoff file and
// transcript, if the handoff still exists.
func claim(state, session string) (file, transcript string, ok bool) {
	claimed := filepath.Join(state, session+".claimed")
	if os.Rename(filepath.Join(state, session+".rearm"), claimed) != nil {
		return "", "", false
	}
	data, err := os.ReadFile(claimed)
	if err != nil {
		return "", "", false
	}
	file, transcript, _ = strings.Cut(strings.TrimRight(string(data), "\n"), "\n")
	if !isFile(file) {
		os.Remove(claimed)
		return "", "", false
	}
	return file, transcript, true
}

// done logs context-guard's outcome for a claimed clear; a failed one is re-armed.
func done(state, session, outcome, text string) bool {
	claimed := filepath.Join(state, session+".claimed")
	if !exists(claimed) {
		return false
	}
	switch outcome {
	case "ok":
		if os.Rename(claimed, filepath.Join(state, session+".cleared")) != nil { // for the statusline
			os.Remove(claimed)
		}
		logWriter(state, session+" "+text)
	case "fail":
		os.Rename(claimed, filepath.Join(state, session+".rearm"))
		logWriter(state, session+" "+text+" (re-armed)")
	default:
		return false
	}
	return true
}

func logWriter(state, line string) {
	f, err := os.OpenFile(filepath.Join(state, "writer.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(time.Now().Format("2006-01-02T15:04:05-07:00") + " " + line + "\n")
}

// writeAndArm is the --write half: run the writer, then arm the clear.
func writeAndArm(session, next, cwd, transcript string) {
	state := stateDir()
	logWriter(state, session+" writer start")
	// Progress markers for the statusline: .writing holds the start epoch while the writer
	// runs; it becomes .failed, or gives way to .rearm / .written. Outcomes of an earlier
	// run go.
	writing := filepath.Join(state, session+".writing")
	for _, s := range []string{"failed", "written", "cleared"} {
		os.Remove(filepath.Join(state, session+"."+s))
	}
	os.WriteFile(writing, []byte(strconv.FormatInt(time.Now().Unix(), 10)+"\n"), 0o644)
	failed := func() {
		os.WriteFile(filepath.Join(state, session+".failed"), nil, 0o644)
		os.Remove(writing)
	}
	cmd, err := writerCommand(transcript, next, cwd, session)
	if err != nil {
		logWriter(state, session+" writer failed: "+err.Error())
		failed()
		return
	}
	if log, err := os.OpenFile(filepath.Join(state, "writer.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		defer log.Close()
		cmd.Stderr = log
	}
	out, err := cmd.Output()
	file := strings.TrimRight(string(out), "\n")
	if err != nil || file == "" {
		logWriter(state, session+" writer failed")
		failed()
		return
	}
	arm(state, session, file, transcript)
	os.Remove(writing)
}

func autoclear(args []string) int {
	if len(args) == 5 && args[0] == "--write" {
		writeAndArm(args[1], args[2], args[3], args[4])
		return 0
	}
	if len(args) == 2 && args[0] == "--claim" {
		if !safeSession.MatchString(args[1]) {
			return 1
		}
		file, transcript, ok := claim(stateDir(), args[1])
		if !ok {
			return 1
		}
		os.Stdout.WriteString(continueLine(file, transcript) + "\n")
		return 0
	}
	if len(args) == 4 && args[0] == "--done" {
		if !safeSession.MatchString(args[1]) || !done(stateDir(), args[1], args[2], args[3]) {
			return 1
		}
		return 0
	}

	// The Stop hook.
	in, ok := readStopInput()
	if !ok || !safeSession.MatchString(in.SessionID) {
		return 0
	}
	// Only the stop that ends the turn the gate's block started.
	if !in.active() {
		return 0
	}
	if st, err := os.Stat(in.Cwd); err != nil || !st.IsDir() {
		return 0
	}
	state := stateDir()
	gateSt, err := os.Stat(filepath.Join(state, in.SessionID+".gate"))
	once := filepath.Join(state, in.SessionID+".autoclear")
	if err != nil || exists(once) {
		return 0
	}
	next := filepath.Join(in.Cwd, ".handoff-next-"+in.SessionID+".md")
	if st, err := os.Stat(next); err == nil && st.Mode().IsRegular() && st.ModTime().After(gateSt.ModTime()) {
		if os.MkdirAll(state, 0o755) == nil && touch(once) == nil {
			startBackground("--write", in.SessionID, next, in.Cwd, in.TranscriptPath)
		}
	}
	return 0
}
