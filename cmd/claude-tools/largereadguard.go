package main

// large-read-guard (PreToolUse(Read) hook): two-tier guard on full-file reads of large
// code files. Estimates tokens from what Read will actually inject (first 2000 lines —
// Read's own cap — at ~4 chars/token). >2k tokens: allow + nudge toward smart-explore or
// offset+limit. >10k tokens: deny with the same redirection. Silent for targeted reads
// (limit given), non-code files, small files, and anything unreadable.
// Port of ~/.claude/hooks/large-read-guard.sh; test-large-read-guard.sh is its suite.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	nudgeTokens = 2000
	denyTokens  = 10000
)

// headLines mirrors `head -n 2000 | wc -c` and `wc -l` in one pass: the bytes up to and
// including the 2000th newline, and the newline count of the whole file.
func headLines(path string) (headBytes, lines int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<16)
	var n int64
	for {
		chunk, err := r.ReadSlice('\n')
		n += int64(len(chunk))
		if len(chunk) > 0 && chunk[len(chunk)-1] == '\n' {
			lines++
			if lines == 2000 {
				headBytes = n
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, 0, err
		}
	}
	if lines < 2000 {
		headBytes = n
	}
	return headBytes, lines, nil
}

func largeReadGuard(args []string) int {
	var in struct {
		SessionID string `json:"session_id"`
		ToolName  string `json:"tool_name"`
		ToolInput struct {
			FilePath string          `json:"file_path"`
			Limit    json.RawMessage `json:"limit"`
		} `json:"tool_input"`
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil || json.Unmarshal(data, &in) != nil || in.ToolName != "Read" {
		return 0
	}
	file := in.ToolInput.FilePath
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	if d, ok := secretFileToolDecision("Read", map[string]any{"file_path": file}, cwd, home, os.Getenv("XDG_CONFIG_HOME")); ok {
		logGuardEvent(in.SessionID, "block", d.Rule)
		fmt.Fprintln(os.Stderr, d.Msg)
		return d.Exit
	}
	if file == "" || !isFile(file) {
		return 0
	}
	if jqRaw(in.ToolInput.Limit, "") != "" {
		return 0 // targeted read — already doing the right thing
	}

	ext := file // ${FILE##*.}: the whole path when there is no dot
	if i := strings.LastIndexByte(file, '.'); i >= 0 {
		ext = file[i+1:]
	}
	switch strings.ToLower(ext) {
	case "md", "markdown", "json", "yaml", "yml", "toml", "ini", "cfg", "conf", "txt": // non-code: Read is fine
		return 0
	case "png", "jpg", "jpeg", "gif", "webp": // images cost by pixels (~1.4k @1280x800), not bytes/4
		return 0
	}

	headBytes, lines, err := headLines(file)
	if err != nil {
		return 0
	}
	tokens := headBytes / 4
	if tokens <= nudgeTokens {
		return 0
	}

	type hookOut struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision,omitempty"`
		PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
		AdditionalContext        string `json:"additionalContext,omitempty"`
	}
	out := hookOut{HookEventName: "PreToolUse"}
	if tokens > denyTokens {
		logGuardEvent(in.SessionID, "block", "large-read")
		out.PermissionDecision = "deny"
		out.PermissionDecisionReason = fmt.Sprintf("Read blocked: %s would inject ~%d tokens (%d lines) into context. Use claude-mem smart-explore for structure, Grep to locate the relevant section, then Read with offset+limit for the ~20-50 lines you need. (large-read-guard hook)", file, tokens, lines)
	} else {
		logGuardEvent(in.SessionID, "nudge", "large-read")
		out.AdditionalContext = fmt.Sprintf("Large read: %s is %d lines (~%d tokens). Consider claude-mem smart-explore for structure, or Read with offset+limit if you only need a specific range. Proceeding.", file, lines, tokens)
	}
	// Same bytes as `jq -cn '{hookSpecificOutput:{...}}'`.
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.Encode(struct {
		HookSpecificOutput hookOut `json:"hookSpecificOutput"`
	}{out})
	return 0
}
