package main

// retry-breaker (PostToolUseFailure + PostToolUse hook on Bash): a circuit breaker for agent
// retry loops. Per session (and per subagent) it counts failed Bash commands and, on the
// 3rd failure of the same normalized command, injects additionalContext quoting the
// exploration budget. A run of different failing commands is probing, not retrying, so it
// never nudges. A success forgets that command's count. Never blocks: exit 0 always.
//
// Wiring: PostToolUseFailure and PostToolUse, matcher Bash, command
// `guardrails retry-breaker`.

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const retryIdentical = 3 // identical failures before the nudge

type retryState struct {
	Counts map[string]int `json:"counts"`
}

func normalizeCmd(cmd string) string {
	n := strings.Join(strings.Fields(cmd), " ")
	if len(n) > 300 {
		n = n[:300]
	}
	return n
}

// observe records one finished Bash command and returns the reminder to inject, if any.
func (s *retryState) observe(cmd string, failed bool) string {
	key := normalizeCmd(cmd)
	if !failed {
		delete(s.Counts, key)
		return ""
	}
	if s.Counts == nil {
		s.Counts = map[string]int{}
	}
	s.Counts[key]++
	if s.Counts[key] != retryIdentical {
		return ""
	}
	return "This exact command has now failed " + strconv.Itoa(retryIdentical) + " times this session. Exploration budget: stop retrying, report what is known, then change search strategy or name the missing evidence. Retrying the same call again is not a new approach."
}

func retryBreaker(args []string) int {
	var in struct {
		Event       string `json:"hook_event_name"`
		SessionID   string `json:"session_id"`
		AgentID     string `json:"agent_id"`
		ToolName    string `json:"tool_name"`
		IsInterrupt bool   `json:"is_interrupt"`
		ToolInput   struct {
			Command string `json:"command"`
		} `json:"tool_input"`
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil || json.Unmarshal(data, &in) != nil || in.ToolName != "Bash" || in.IsInterrupt ||
		!safeSession.MatchString(in.SessionID) {
		return 0
	}
	if in.Event != "PostToolUseFailure" && in.Event != "PostToolUse" {
		return 0
	}
	name := in.SessionID
	if in.AgentID != "" {
		if !safeSession.MatchString(in.AgentID) {
			return 0
		}
		name += "." + in.AgentID
	}
	file := filepath.Join(stateDir(), name+".retry")
	failed := in.Event == "PostToolUseFailure"

	var st retryState
	b, readErr := os.ReadFile(file)
	if readErr == nil {
		json.Unmarshal(b, &st)
	} else if !failed {
		return 0 // success and nothing recorded: nothing to reset, no file to create
	}
	msg := st.observe(in.ToolInput.Command, failed)
	if out, err := json.Marshal(st); err == nil {
		os.MkdirAll(filepath.Dir(file), 0o755)
		os.WriteFile(file, out, 0o644)
	}
	if msg != "" {
		logGuardEvent(in.SessionID, "nudge", "retry-breaker")
		emitContext(os.Stdout, "PostToolUseFailure", msg)
	}
	return 0
}
