package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
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

// guardHintMessages returns the guard messages that point the model at other tools: the
// large-read nudge and block, and the raw-client-data ask.
func guardHintMessages(t *testing.T) []string {
	t.Helper()
	var msgs []string
	for _, lines := range []int{300, 1500} {
		p := filepath.Join(t.TempDir(), "big.go")
		os.WriteFile(p, []byte(strings.Repeat("// a line of Go source code padded out to be long\n", lines)), 0o644)
		b, _ := json.Marshal(map[string]any{"session_id": "s", "tool_name": "Read", "tool_input": map[string]any{"file_path": p}})
		_, out, _ := runHook(t, largeReadGuard, string(b))
		msgs = append(msgs, out)
	}
	return append(msgs, rawClientDataRule(`python3 q.py scratch/clients.parquet`).Msg)
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
