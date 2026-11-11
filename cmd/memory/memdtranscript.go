package main

// memory transcript parser: turns Claude Code transcript JSONL lines into prompt, tool_use and
// turn_end events. Lines are Claude Code transcript JSONL.

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"time"
)

// memdEvent is one captured event. Input, Output and PromptText are redacted.
type memdEvent struct {
	Kind                                           string // "prompt" | "tool_use" | "turn_end"
	SessionID, Account, Project, Cwd               string
	ToolUseID, ToolName, Input, Output, PromptText string
	At                                             time.Time
	Observer, Sub                                  bool
	Usage                                          *memdUsage // Kind "usage" only
	ReplyKey, ReplyText                            string     // Kind "reply": line uuid#block, masked text
	GitBranch, Model                               string     // from the line; empty when it carries none
	cap                                            captureDecision
}

// memdFileState is the per-file state parseMemdLines carries between calls. Observer and Sub
// latch on: set from the path, then by any line with an sdk* entrypoint or
// isSidechain, and never cleared. Pending holds tool_uses awaiting their tool_result, keyed
// by tool_use_id.
type memdFileState struct {
	Pending       map[string]*memdEvent
	Observer, Sub bool
	Reset         string // "clear" or "compact" until the next main-thread response takes it
}

// parseMemdLines parses one batch of transcript lines in order. The caller keeps one
// memdFileState per transcript file across calls (nil is allowed and means a fresh one). A
// tool_use becomes an event only when its tool_result line arrives, possibly in a later
// batch; its At is the tool_use line's time. Every emitted event carries st.Observer and
// st.Sub as latched at emit time, so a signal seen after the tool_use still applies. Lines
// that are not JSON objects are skipped silently; they never fail the batch.
func parseMemdLines(path, account string, lines [][]byte, st *memdFileState) (events []memdEvent) {
	if st == nil {
		st = &memdFileState{}
	}
	if st.Pending == nil {
		st.Pending = map[string]*memdEvent{}
	}
	pending := st.Pending
	st.Observer = st.Observer || strings.Contains(path, "memory-observer-cwd")
	st.Sub = st.Sub || strings.Contains(path, "/subagents/")
	for _, raw := range lines {
		v, err := decodeJSON(raw)
		if err != nil {
			continue
		}
		d, ok := v.(*obj)
		if !ok {
			continue
		}
		if strings.HasPrefix(d.getStr("entrypoint"), "sdk") {
			st.Observer = true
		}
		if truthy(d.get("isSidechain")) {
			st.Sub = true
		}
		if u := usageFromLine(path, d, st); u != nil {
			events = append(events, memdEvent{Kind: "usage", SessionID: u.SessionID, At: u.At, Usage: u})
		}
		msg := d.getObj("message")
		if msg == nil {
			continue
		}
		base := memdEvent{
			SessionID: d.getStr("sessionId"),
			Account:   account,
			Cwd:       d.getStr("cwd"),
			GitBranch: d.getStr("gitBranch"),
		}
		if base.SessionID == "" {
			name := filepath.Base(path)
			base.SessionID = strings.TrimSuffix(name, filepath.Ext(name))
		}
		base.Project = projectKey(base.Cwd)
		if ts := d.getStr("timestamp"); ts != "" {
			base.At, _ = time.Parse(time.RFC3339Nano, ts)
		}
		content, _ := msg.get("content").([]any)
		switch d.get("type") {
		case "assistant":
			if m := msg.getStr("model"); m != "<synthetic>" {
				base.Model = m
			}
			for i, x := range content {
				b, ok := x.(*obj)
				if ok && b.get("type") == "text" {
					if e, ok := replyEvent(base, d, msg, i, b, st); ok {
						events = append(events, e)
					}
					continue
				}
				if !ok || b.get("type") != "tool_use" {
					continue
				}
				id := b.getStr("id")
				if id == "" {
					continue
				}
				e := base
				e.Kind, e.ToolUseID, e.ToolName = "tool_use", id, b.getStr("name")
				if in := b.getObj("input"); in != nil {
					if cmd, ok := in.vals["command"].(string); ok && e.ToolName == "Bash" {
						in.vals["command"] = cmdNorm(cmd)
					}
				}
				raw := memdJSON(b.get("input"))
				e.cap = captureFor(captureCfg, base.Cwd, e.ToolName, raw)
				e.Input = e.cap.input(memdRedact(raw))
				pending[id] = &e
			}
			if msg.get("stop_reason") == "end_turn" {
				e := base
				e.Observer, e.Sub = st.Observer, st.Sub
				e.Kind = "turn_end"
				events = append(events, e)
			}
		case "user":
			for _, x := range content {
				b, ok := x.(*obj)
				if !ok || b.get("type") != "tool_result" {
					continue
				}
				id := b.getStr("tool_use_id")
				if e, ok := pending[id]; ok {
					e.Output = e.cap.output(memdRedact(textOf(b.get("content"))), truthy(b.get("is_error")))
					e.Observer, e.Sub = st.Observer, st.Sub
					events = append(events, *e)
					delete(pending, id)
				}
			}
			if p := typedPrompt(d, msg); p != "" {
				e := base
				e.Kind, e.PromptText = "prompt", memdRedact(p)
				e.Observer, e.Sub = st.Observer, st.Sub
				events = append(events, e)
			}
		}
	}
	return events
}

// memdJSON is compact JSON of a decoded value with key order kept and no HTML escaping, so
// the observer reads the tool input as written.
func memdJSON(v any) string {
	var b strings.Builder
	var enc func(any)
	str := func(x any) {
		var sb strings.Builder
		e := json.NewEncoder(&sb)
		e.SetEscapeHTML(false)
		e.Encode(x)
		b.WriteString(strings.TrimRight(sb.String(), "\n"))
	}
	enc = func(v any) {
		switch t := v.(type) {
		case *obj:
			b.WriteByte('{')
			for i, k := range t.keys {
				if i > 0 {
					b.WriteByte(',')
				}
				str(k)
				b.WriteByte(':')
				enc(t.vals[k])
			}
			b.WriteByte('}')
		case []any:
			b.WriteByte('[')
			for i, x := range t {
				if i > 0 {
					b.WriteByte(',')
				}
				enc(x)
			}
			b.WriteByte(']')
		case json.Number:
			b.WriteString(t.String())
		default: // string, bool, nil
			str(t)
		}
	}
	enc(v)
	return b.String()
}
