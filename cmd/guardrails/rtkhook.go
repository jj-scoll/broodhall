package main

// Opt-in RTK rewrite. RTK is a token-saving command rewriter with its own Claude Code hook.
// Claude Code runs PreToolUse hooks in parallel and keeps the last updatedInput, so a second
// rtk hook would race this plugin's PII wrap. With the plugin's `rtk` option on, pre-tool-use
// runs `rtk hook claude` itself, after every guard and only when no mask has claimed the
// command (see preToolUseRun). Off by default.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path"
	"strings"
	"time"
)

// rtkHook is what preToolUseRun calls; tests replace it so they never run a real rtk.
var rtkHook = runRTK

// rtkEnabled reads GUARDRAILS_RTK (or CLAUDE_TOOLS_RTK), which the launcher sets from the
// `rtk` plugin option.
func rtkEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(settingEnv("RTK"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// runRTK runs RTK's own Claude Code hook on the same input and returns its stdout, stderr
// and exit code. Disabled, missing or hung rtk only loses the token-saving rewrite: exit 0,
// never a block. Only rtk's own exit 2 (a deny rule) is forwarded. A non-empty stderr with
// exit 0 is a note for the user.
func runRTK(stdin []byte) ([]byte, []byte, int) {
	if !rtkEnabled() {
		return nil, nil, 0
	}
	name := strings.TrimSpace(settingEnv("RTK_CMD"))
	if name == "" {
		name = "rtk"
	}
	bin, err := exec.LookPath(name)
	if err != nil {
		return nil, []byte(rtkMissingNote(stdin, name)), 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, bin, "hook", "claude")
	c.Stdin = bytes.NewReader(stdin)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	err = c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 2 {
		return nil, errb.Bytes(), 2
	}
	if err != nil {
		return nil, nil, 0
	}
	return out.Bytes(), nil, 0
}

// rtkMissingNote is the one stderr line for an enabled but missing rtk, once per session
// (the mark lives in the guard state file); "" after that.
func rtkMissingNote(stdin []byte, name string) string {
	var in struct {
		SessionID string `json:"session_id"`
	}
	json.Unmarshal(stdin, &in)
	if st := newFileState(in.SessionID); st != nil {
		now := time.Now()
		if st.Seen("rtk-missing", 24*time.Hour, now) {
			return ""
		}
		st.Mark("rtk-missing", now)
	}
	return "guardrails: the rtk option is on but " + name + " was not found; commands run without the rtk rewrite.\n"
}

// gitRetargets reports whether any git command in cmd moves git's target with -C,
// --git-dir or --work-tree. rtk would wrap such a call as `rtk git -C ...`, which hides
// the target from guards that look for it, so preToolUseRun leaves these commands alone.
func gitRetargets(cmd string) bool {
	return shellFind(cmd, func(w []string) string {
		if path.Base(w[0]) != "git" {
			return ""
		}
		for i := 1; i < len(w) && strings.HasPrefix(w[i], "-"); i++ {
			a := w[i]
			if a == "-C" || a == "--git-dir" || a == "--work-tree" ||
				strings.HasPrefix(a, "--git-dir=") || strings.HasPrefix(a, "--work-tree=") {
				return "x"
			}
			if gitArgOpts[a] {
				i++
			}
		}
		return ""
	}, func(cmd string) string {
		// Cannot parse: any command naming git and one of the flags counts.
		if strings.Contains(cmd, "git") {
			for _, f := range []string{" -C", "--git-dir", "--work-tree"} {
				if strings.Contains(cmd, f) {
					return "x"
				}
			}
		}
		return ""
	}) != ""
}
