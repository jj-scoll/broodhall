package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// TestMain keeps every test's guard state and ledger out of the real /tmp/claude-handoff;
// tests that check state set their own HANDOFF_STATE_DIR on top.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "claude-tools-test-")
	if err != nil {
		panic(err)
	}
	os.Setenv("HANDOFF_STATE_DIR", dir)
	os.Setenv("CLAUDE_MEM_DATA_DIR", dir) // no settings.json: memd keeps claude-mem's default skip lists
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

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

// bashInput builds a PreToolUse/Post* hook payload for a Bash command.
func bashInput(event, session, cwd, cmd string, extra string) string {
	q := func(s string) string {
		r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
		return `"` + r.Replace(s) + `"`
	}
	return `{"hook_event_name":` + q(event) + `,"session_id":` + q(session) + `,"cwd":` + q(cwd) +
		`,"tool_name":"Bash","tool_input":{"command":` + q(cmd) + `}` + extra + `}`
}
