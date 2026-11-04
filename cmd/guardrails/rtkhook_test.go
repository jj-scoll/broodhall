package main

// The opt-in RTK rewrite: pre-tool-use runs `rtk hook claude` itself (Claude Code runs
// PreToolUse hooks in parallel and keeps the last updatedInput, so a separate rtk hook
// would race this one). These tests put a fake rtk script on PATH and run runRTK, the real
// rtkHook, through preToolUseRun.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fakeRTKReply = `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","updatedInput":{"command":"rtk REWRITTEN"}}}`

// fakeRTK installs an rtk script that logs its args and stdin (shell builtins only: PATH is just this dir) to <dir>/log and prints
// fakeRTKReply. It returns the dir (for PATH) and the log path.
func fakeRTK(t *testing.T, name string) (dir, log string) {
	t.Helper()
	dir = t.TempDir()
	log = filepath.Join(dir, "log")
	body := "#!/bin/sh\necho \"args=$*\" >>" + log + "\nwhile IFS= read -r l || [ -n \"$l\" ]; do echo \"$l\"; done >>" + log + "\nprintf '%s' '" + fakeRTKReply + "'\n"
	writeExe(t, filepath.Join(dir, name), body)
	return dir, log
}

// runRTKHook runs one Bash call through preToolUseRun with the real rtk runner and the
// given RTK env, with PATH limited to pathDir.
func runRTKHook(t *testing.T, env map[string]string, pathDir, cmd string) (code int, stdout, stderr string) {
	t.Helper()
	t.Setenv("PATH", pathDir)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
	t.Setenv("CLAUDE_TOOLS_OFF", "")
	t.Setenv("CLAUDE_TOOLS_RTK", "")
	t.Setenv("CLAUDE_TOOLS_RTK_CMD", "")
	for k, v := range env {
		t.Setenv(k, v)
	}
	return callRTKHook(t, cmd)
}

func callRTKHook(t *testing.T, cmd string) (int, string, string) {
	t.Helper()
	in, _ := json.Marshal(map[string]any{"session_id": "sess1", "cwd": t.TempDir(), "tool_name": "Bash",
		"tool_input": map[string]any{"command": cmd, "description": "d"}})
	var stdout, stderr bytes.Buffer
	code := preToolUseRun(in, "CT", runRTK, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func readLog(t *testing.T, log string) string {
	t.Helper()
	b, _ := os.ReadFile(log)
	return string(b)
}

func TestRTKEnabledRewrites(t *testing.T) {
	dir, log := fakeRTK(t, "rtk")
	code, out, errOut := runRTKHook(t, map[string]string{"CLAUDE_TOOLS_RTK": "true"}, dir, "git status")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if strings.TrimSpace(out) != fakeRTKReply {
		t.Errorf("stdout = %s", out)
	}
	l := readLog(t, log)
	if !strings.Contains(l, "args=hook claude") || !strings.Contains(l, `"command":"git status"`) {
		t.Errorf("rtk saw %q", l)
	}
}

func TestRTKDisabledByDefault(t *testing.T) {
	dir, log := fakeRTK(t, "rtk")
	for _, v := range []string{"", "false", "0", "no"} {
		code, out, errOut := runRTKHook(t, map[string]string{"CLAUDE_TOOLS_RTK": v}, dir, "git status")
		if code != 0 || out != "" || errOut != "" || readLog(t, log) != "" {
			t.Errorf("RTK=%q: exit %d, stdout %q, stderr %q, rtk ran=%v", v, code, out, errOut, readLog(t, log) != "")
		}
	}
}

func TestRTKMissingNotesOnceAndNeverBlocks(t *testing.T) {
	empty := t.TempDir()
	env := map[string]string{"CLAUDE_TOOLS_RTK": "1"}
	code, out, errOut := runRTKHook(t, env, empty, "git status")
	if code != 0 || out != "" {
		t.Fatalf("missing rtk: exit %d, stdout %q", code, out)
	}
	if strings.Count(errOut, "rtk") == 0 || strings.Count(strings.TrimSpace(errOut), "\n") != 0 {
		t.Errorf("want exactly one stderr line naming rtk, got %q", errOut)
	}
	// Same session, same state dir: no second note.
	if code, out, errOut = callRTKHook(t, "git status"); code != 0 || out != "" || errOut != "" {
		t.Errorf("second call: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	// The guards still run.
	if code, _, _ = callRTKHook(t, "git push --force origin main"); code != 2 {
		t.Errorf("guards must still block with rtk missing, exit %d", code)
	}
}

func TestRTKCommandOverride(t *testing.T) {
	dir, log := fakeRTK(t, "my-rtk")
	path := filepath.Join(dir, "my-rtk")
	code, out, _ := runRTKHook(t, map[string]string{"CLAUDE_TOOLS_RTK": "1", "CLAUDE_TOOLS_RTK_CMD": path}, t.TempDir(), "git status")
	if code != 0 || strings.TrimSpace(out) != fakeRTKReply || !strings.Contains(readLog(t, log), "args=hook claude") {
		t.Errorf("exit %d, stdout %q, log %q", code, out, readLog(t, log))
	}
}

// PII wrap and secret mask own the command: rtk never runs for them, so the parallel-hook
// race cannot pick rtk's rewrite over the mask.
func TestRTKOrderingPIIWrapWins(t *testing.T) {
	dir, log := fakeRTK(t, "rtk")
	env := map[string]string{"CLAUDE_TOOLS_RTK": "1"}
	code, out, _ := runRTKHook(t, env, dir, `psql -t -A -c "select email from users"`)
	if code != 0 || !strings.Contains(out, "CT redact --pii") || strings.Contains(out, "REWRITTEN") || readLog(t, log) != "" {
		t.Errorf("exit %d, rtk ran=%v, out %s", code, readLog(t, log) != "", out)
	}
	// pii family off: no wrap, so rtk gets the command.
	env["CLAUDE_TOOLS_OFF"] = "pii"
	_, out, _ = runRTKHook(t, env, dir, `psql -c "select 1"`)
	if !strings.Contains(out, "REWRITTEN") || readLog(t, log) == "" {
		t.Errorf("pii off: want rtk rewrite, got %s", out)
	}
}

func TestRTKOrderingGuardsSeeOriginalCommand(t *testing.T) {
	dir, log := fakeRTK(t, "rtk")
	env := map[string]string{"CLAUDE_TOOLS_RTK": "1"}
	// A block or ask on the original command wins, and rtk never runs.
	code, _, _ := runRTKHook(t, env, dir, "git -C /some/repo push --force origin main")
	if code != 2 || readLog(t, log) != "" {
		t.Errorf("force push via git -C: exit %d, rtk ran=%v", code, readLog(t, log) != "")
	}
	code, out, _ := runRTKHook(t, env, dir, "git -C /some/repo branch -D old")
	if code != 0 || !strings.Contains(out, `"permissionDecision": "ask"`) || readLog(t, log) != "" {
		t.Errorf("branch -D via git -C: exit %d, rtk ran=%v, out %s", code, readLog(t, log) != "", out)
	}
}

// The rewrite must not hide a git target from other guards (a worktree-isolation guard
// refuses `rtk git ...` because it cannot see the target through the wrapper): a command
// that retargets git is left as typed.
func TestRTKNeverWrapsRetargetedGit(t *testing.T) {
	dir, log := fakeRTK(t, "rtk")
	env := map[string]string{"CLAUDE_TOOLS_RTK": "1"}
	for _, cmd := range []string{
		"git -C /some/repo status",
		"git --git-dir=/r/.git log",
		"git --work-tree /r status",
		"cd /x && git -C ../y diff | head",
		`bash -c 'git -C /y status'`,
	} {
		code, out, errOut := runRTKHook(t, env, dir, cmd)
		if code != 0 || out != "" || errOut != "" || readLog(t, log) != "" {
			t.Errorf("%q: exit %d, stdout %q, stderr %q, rtk ran=%v", cmd, code, out, errOut, readLog(t, log) != "")
		}
	}
	// Plain git, and -C on another tool, are still rewritten.
	for _, cmd := range []string{"git status", "make -C build"} {
		if _, out, _ := runRTKHook(t, env, dir, cmd); !strings.Contains(out, "REWRITTEN") {
			t.Errorf("%q: want rtk rewrite, got %q", cmd, out)
		}
	}
}

func TestGitRetargets(t *testing.T) {
	tests := []struct {
		cmd  string
		want bool
	}{
		{"git status", false},
		{"git -C d status", true},
		{"git -c core.x=1 status", false},
		{"git --git-dir=d status", true},
		{"/usr/bin/git --work-tree=d status", true},
		{"echo git -C d", false},
		{"make -C d", false},
		{"git status && git -C d log", true},
	}
	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			if got := gitRetargets(tt.cmd); got != tt.want {
				t.Errorf("gitRetargets(%q) = %v", tt.cmd, got)
			}
		})
	}
}
