package main

// read-dedup (PreToolUse(Read) hook): soft-nudge on redundant re-reads of unchanged files.
// Emits hookSpecificOutput.additionalContext (model-visible, shown next to the tool
// result) and lets the Read proceed. Never blocks. Per-session state in /tmp (compact-guard
// clears it on compaction); entries expire after 30 minutes.
// Port of ~/.claude/hooks/read-dedup.sh; test-read-dedup.sh is its suite.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// jqRaw mirrors `jq -r '.x // "<def>"'` for a scalar: missing, null and false give def,
// strings print raw, numbers as written.
func jqRaw(v json.RawMessage, def string) string {
	s := strings.TrimSpace(string(v))
	switch {
	case s == "" || s == "null" || s == "false":
		return def
	case s[0] == '"':
		var str string
		if json.Unmarshal(v, &str) == nil {
			return str
		}
	}
	return s
}

// readCachePath is the per-session ledger of reads. compact-guard removes it when the
// session compacts, because Claude no longer holds those reads.
func readCachePath(session string) string { return "/tmp/claude-readcache-" + session }

func readDedup(args []string) int {
	var in struct {
		SessionID json.RawMessage `json:"session_id"`
		ToolName  string          `json:"tool_name"`
		Cwd       string          `json:"cwd"`
		AgentID   string          `json:"agent_id"`
		AgentID2  string          `json:"agentId"`
		ToolInput struct {
			FilePath string          `json:"file_path"`
			Offset   json.RawMessage `json:"offset"`
			Limit    json.RawMessage `json:"limit"`
		} `json:"tool_input"`
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil || json.Unmarshal(data, &in) != nil || in.ToolName != "Read" || in.ToolInput.FilePath == "" {
		return 0
	}
	file := in.ToolInput.FilePath
	// Same check as compact-guard, so the ledger path is safe and compaction can clear it.
	session := jqRaw(in.SessionID, "")
	if !safeSession.MatchString(session) {
		return 0
	}

	// stat -c %Y / %s: no symlink follow.
	st, err := os.Lstat(file)
	if err != nil {
		return 0
	}
	key := fmt.Sprintf("%s|%d|%s|%s", file, st.ModTime().Unix(),
		jqRaw(in.ToolInput.Offset, "-"), jqRaw(in.ToolInput.Limit, "-"))
	state := readCachePath(session)
	now := time.Now().Unix()
	cutoff := now - 1800 // 30 minutes

	var kept strings.Builder
	var matchEpoch int64
	matched := false
	if b, err := os.ReadFile(state); err == nil {
		lines := strings.Split(string(b), "\n")
		// `while read` drops a last line with no newline; Split leaves it as the final element.
		for _, line := range lines[:len(lines)-1] {
			es, k, _ := strings.Cut(line, "\t")
			if es == "" || k == "" || !digits.MatchString(es) {
				continue
			}
			epoch, err := strconv.ParseInt(es, 10, 64)
			if err != nil || epoch < cutoff { // drop expired entries
				continue
			}
			if k == key {
				matchEpoch, matched = epoch, true // remember first-seen time; refresh below
				continue
			}
			fmt.Fprintf(&kept, "%s\t%s\n", es, k)
		}
	}
	fmt.Fprintf(&kept, "%d\t%s\n", now, key) // record/refresh this read
	if tmp, err := os.CreateTemp("/tmp", "tmp."); err == nil {
		tmp.WriteString(kept.String())
		tmp.Close()
		if os.Rename(tmp.Name(), state) != nil {
			os.Remove(tmp.Name())
		}
	}

	if !matched {
		// First read in this window and nothing to nudge about: add what memory knows.
		if t := memdLookupFile(in.Cwd, file, in.AgentID != "" || in.AgentID2 != ""); t != "" {
			emitContext(os.Stdout, "PreToolUse", t)
		}
		return 0
	}
	logGuardEvent(session, "nudge", "read-dedup")
	msg := fmt.Sprintf("You already read %s at %s this session and it is unchanged; the content is likely still in context above. Re-reading repeats roughly %d tokens — skip unless you need a fresh look.",
		file, time.Unix(matchEpoch, 0).Format("15:04"), st.Size()/4)
	type hookOut struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	}
	// Same bytes as `jq -n '{hookSpecificOutput:{...}}'`.
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	enc.Encode(struct {
		HookSpecificOutput hookOut `json:"hookSpecificOutput"`
	}{hookOut{"PreToolUse", msg}})
	return 0
}
