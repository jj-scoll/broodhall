package main

// gate (Stop hook): once per session (re-armed by compact-guard on each compaction), when
// the context passes the handoff threshold, block the stop once and ask Claude to write a
// handoff with the memory plugin's handoff skill before it ends the turn.
//
// Threshold: HANDOFF_TOKENS, else 80% of CLAUDE_CODE_AUTO_COMPACT_WINDOW (default 300000),
// so the handoff lands before auto-compact. Context size is the latest assistant usage in
// the transcript. Every error path allows the stop (exit 0, no output): the gate is advice,
// and a hook that cannot read its input must not trap a session in a block loop.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// stopInput is the Stop hook payload, as far as the gate reads it.
type stopInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	StopHookActive any    `json:"stop_hook_active"`
}

// active reports whether this stop already follows a block by a Stop hook.
func (in stopInput) active() bool {
	return in.StopHookActive == true || in.StopHookActive == "true"
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// envInt reads an integer env var, else def (unset, empty or not a number all give def).
func envInt(name string, def int64) int64 {
	if n, err := strconv.ParseInt(os.Getenv(name), 10, 64); err == nil {
		return n
	}
	return def
}

func touch(p string) error {
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	f.Close()
	now := time.Now()
	return os.Chtimes(p, now, now)
}

// lastContextTokens returns the context size of the latest assistant entry with usage, from
// the last 4MB of the transcript only (transcripts reach tens of MB). Lines that do not
// parse (the one cut at the start of the window, a partly written last one) are skipped.
func lastContextTokens(path string) (int64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	const window = 4_000_000
	if st, err := f.Stat(); err == nil && st.Size() > window {
		f.Seek(st.Size()-window, io.SeekStart)
	}
	var tokens int64
	found := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), window+1)
	for sc.Scan() {
		var e struct {
			Type    string `json:"type"`
			Message struct {
				Usage *struct {
					Input       int64 `json:"input_tokens"`
					CacheRead   int64 `json:"cache_read_input_tokens"`
					CacheCreate int64 `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Type != "assistant" || e.Message.Usage == nil {
			continue
		}
		u := e.Message.Usage
		tokens, found = u.Input+u.CacheRead+u.CacheCreate, true
	}
	return tokens, found
}

func gate(args []string) int {
	var in stopInput
	data, err := io.ReadAll(os.Stdin)
	if err != nil || json.Unmarshal(data, &in) != nil ||
		in.active() || !safeSession.MatchString(in.SessionID) || !isFile(in.TranscriptPath) {
		return 0
	}
	state := stateDir()
	mark := filepath.Join(state, in.SessionID+".gate")
	if exists(mark) {
		return 0
	}
	window := envInt("CLAUDE_CODE_AUTO_COMPACT_WINDOW", 300000)
	threshold := envInt("HANDOFF_TOKENS", window*4/5)
	tokens, found := lastContextTokens(in.TranscriptPath)
	if !found || tokens < threshold {
		return 0
	}
	if os.MkdirAll(state, 0o755) != nil || touch(mark) != nil {
		return 0
	}
	reason := fmt.Sprintf("Context is at %d tokens (handoff threshold %d). Before ending this turn, run the memory:handoff skill to write a handoff for this work, then end the turn with one line saying the handoff is written. The next session in this project is shown it once at its start, so the user can /clear and continue. If the user is not present, keep working: auto-compact will carry the facts forward, and this gate re-arms after it.",
		tokens, threshold)
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	enc.Encode(struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}{"block", reason})
	return 0
}
