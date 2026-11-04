package main

// session-env (SessionStart hook, opt-in): appends `export AWS_SDK_UA_APP_ID=claude-<first 8 chars
// of the session id>` to $CLAUDE_ENV_FILE, so every aws CLI / boto3 call the session's Bash
// commands make carries `app/claude-<id>` in its User-Agent and CloudTrail's userAgent
// names the session. Docs: SessionStart hooks get CLAUDE_ENV_FILE; write `export` lines
// with >> so other hooks' lines survive. SessionStart stdout would land in Claude's
// context, so this prints nothing, and it never fails the session. It does nothing unless the
// aws_session_tag plugin option is "true" (CLAUDE_PLUGIN_OPTION_AWS_SESSION_TAG), or, when
// that option is unset, GUARDRAILS_AWS_SESSION_TAG (or CLAUDE_TOOLS_AWS_SESSION_TAG) is "1" or "true".

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

const uaAppIDVar = "AWS_SDK_UA_APP_ID"

// sessionEnvEnabled reports whether the user opted in. A set plugin option decides alone.
func sessionEnvEnabled() bool {
	v, ok := os.LookupEnv("CLAUDE_PLUGIN_OPTION_AWS_SESSION_TAG")
	if !ok || v == "" {
		v = settingEnv("AWS_SESSION_TAG")
	}
	return v == "true" || v == "1"
}

func sessionEnv(args []string) int {
	if !sessionEnvEnabled() {
		return 0
	}
	if err := writeSessionEnv(os.Stdin, os.Getenv("CLAUDE_ENV_FILE")); err != nil {
		fmt.Fprintln(os.Stderr, "session-env:", err)
	}
	return 0
}

func writeSessionEnv(r io.Reader, envFile string) error {
	if envFile == "" {
		return nil
	}
	var in struct {
		SessionID string `json:"session_id"`
	}
	data, err := io.ReadAll(r)
	if err != nil || json.Unmarshal(data, &in) != nil {
		return nil
	}
	if len(in.SessionID) < 8 || !safeSession.MatchString(in.SessionID) {
		return nil
	}
	line := "export " + uaAppIDVar + "=claude-" + in.SessionID[:8]
	existing, _ := os.ReadFile(envFile)
	sc := bufio.NewScanner(strings.NewReader(string(existing)))
	for sc.Scan() {
		if sc.Text() == line {
			return nil // resume or compaction re-ran the hook with the same id
		}
	}
	f, err := os.OpenFile(envFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		line = "\n" + line
	}
	_, err = f.WriteString(line + "\n")
	return err
}
