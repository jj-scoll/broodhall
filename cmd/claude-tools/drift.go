package main

// driftVerdict: statusline's drift tripwire, ported from ~/.claude/drift-check.py so it runs
// in process instead of as a python3 subprocess (~20 ms of a ~32 ms render). It counts
// repeated tool calls (re-reading a file, re-running a command, churning one file with
// edits) in the last 60 calls of the transcript's last 500,000 bytes and returns one
// segment such as "drift Read x4 cmap.py +2" when the session looks circular, "" when clean.
// Where the script would crash on an oddly shaped entry (and print nothing at all), this
// skips the entry.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

const (
	driftWindow = 60      // tool_use blocks to look back over
	driftTail   = 500_000 // bytes of transcript to read
)

// flag when a count exceeds the limit
var driftLimits = map[string]int{"Read": 2, "Bash": 2, "Edit": 3}

var driftKinds = map[string]string{"Read": "Read", "NotebookRead": "Read", "Bash": "Bash",
	"Edit": "Edit", "Write": "Edit", "NotebookEdit": "Edit"}

type driftCall struct{ kind, label string }

// pyTruthy is Python's truth test on a decoded JSON value.
func pyTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

// driftCalls lists (kind, label) per main-thread tool_use in the transcript tail, oldest first.
func driftCalls(path string) []driftCall {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	start := max(0, st.Size()-driftTail)
	blob := make([]byte, st.Size()-start)
	if _, err := f.ReadAt(blob, start); err != nil {
		return nil
	}
	lines := bytes.Split(blob, []byte("\n"))
	if start > 0 {
		lines = lines[1:] // the first line is a fragment
	}
	var out []driftCall
	for _, line := range lines {
		if !bytes.Contains(line, []byte(`"tool_use"`)) {
			continue
		}
		var d map[string]any
		if json.Unmarshal(line, &d) != nil || pyTruthy(d["isSidechain"]) { // subagent loop, not ours
			continue
		}
		msg, _ := d["message"].(map[string]any)
		content, _ := msg["content"].([]any)
		for _, b := range content {
			blk, _ := b.(map[string]any)
			if blk["type"] != "tool_use" {
				continue
			}
			name, _ := blk["name"].(string)
			kind := driftKinds[name]
			if kind == "" {
				continue
			}
			inp, _ := blk["input"].(map[string]any)
			var label string
			if kind == "Bash" {
				cmd, _ := inp["command"].(string)
				label = strings.Join(strings.Fields(cmd), " ")
			} else if p, _ := inp["file_path"].(string); p != "" {
				label = p
			} else {
				label, _ = inp["notebook_path"].(string)
			}
			if label != "" {
				out = append(out, driftCall{kind, label})
			}
		}
	}
	return out
}

func driftVerdict(path string) string {
	calls := driftCalls(path)
	if len(calls) > driftWindow {
		calls = calls[len(calls)-driftWindow:]
	}
	counts := map[driftCall]int{}
	for _, c := range calls {
		counts[c]++
	}
	type hit struct {
		c driftCall
		n int
	}
	var over []hit
	for c, n := range counts {
		if n > driftLimits[c.kind] {
			over = append(over, hit{c, n})
		}
	}
	if len(over) == 0 {
		return ""
	}
	// the script's reverse sort of (excess, count, kind, label) tuples
	sort.Slice(over, func(i, j int) bool {
		a, b := over[i], over[j]
		if ea, eb := a.n-driftLimits[a.c.kind], b.n-driftLimits[b.c.kind]; ea != eb {
			return ea > eb
		}
		if a.n != b.n {
			return a.n > b.n
		}
		if a.c.kind != b.c.kind {
			return a.c.kind > b.c.kind
		}
		return a.c.label > b.c.label
	})
	top := over[0]
	short := top.c.label
	if top.c.kind != "Bash" {
		short = short[strings.LastIndex(short, "/")+1:]
	}
	if r := []rune(short); len(r) > 28 {
		short = string(r[:27]) + "…"
	}
	extra := ""
	if len(over) > 1 {
		extra = fmt.Sprintf(" +%d", len(over)-1)
	}
	return fmt.Sprintf("drift %s x%d %s%s", top.c.kind, top.n, short, extra)
}
