package main

// Transcript reading shared by the handoff writer's digest and facts block: ports of
// ~/.claude/hooks/sessiondb/ingest.py text_of and typed_prompt, and of the read loop both
// handoff_writer.digest and facts.build open with.

import (
	"fmt"
	"strings"
)

// jsonLines is `for line in open(path, errors="replace")` plus json.loads: every line that
// parses, whatever it parses to.
func jsonLines(path string) ([]any, error) {
	text, err := readPyText(path, true)
	if err != nil {
		return nil, err
	}
	var out []any
	for _, line := range strings.SplitAfter(text, "\n") {
		if v, err := decodeJSON([]byte(line)); err == nil {
			out = append(out, v)
		}
	}
	return out, nil
}

// transcriptLines is jsonLines for callers that go on to call d.get(...) on each line: one that
// parses to something other than an object stops it, as d.get raises in Python.
func transcriptLines(path string) ([]*obj, error) {
	values, err := jsonLines(path)
	if err != nil {
		return nil, err
	}
	var out []*obj
	for _, v := range values {
		d, ok := v.(*obj)
		if !ok {
			return nil, fmt.Errorf("%s: a line is JSON but not an object", path)
		}
		out = append(out, d)
	}
	return out, nil
}

// textOf is ingest.text_of: a string as is, a list's text blocks (and non-dict items) joined.
func textOf(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, b := range c {
			o, ok := b.(*obj)
			if !ok {
				parts = append(parts, pyStr(b))
				continue
			}
			if t := o.get("type"); t == nil || t == "text" {
				parts = append(parts, getStr(o, "text", ""))
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// typedPrompt is ingest.typed_prompt: the text of a prompt the person typed, else "".
func typedPrompt(d, msg *obj) string {
	if truthy(d.get("isMeta")) {
		return ""
	}
	content := msg.get("content")
	if list, ok := content.([]any); ok {
		for _, b := range list {
			if o, ok := b.(*obj); ok && o.get("type") == "tool_result" {
				return ""
			}
		}
	}
	text := pyStrip(textOf(content))
	if origin, ok := d.get("origin").(*obj); ok {
		if origin.get("kind") == "human" {
			return text
		}
		return ""
	}
	if strings.HasPrefix(text, "<") {
		return ""
	}
	return text
}

// getStr is str(o.get(k, def)), with def when k is absent.
func getStr(o *obj, k, def string) string {
	if v, ok := o.lookup(k); ok {
		return pyStr(v)
	}
	return def
}

// stamp is (d.get("timestamp") or "")[:19].
func stamp(d *obj) string {
	if ts := d.get("timestamp"); truthy(ts) {
		return cut(pyStr(ts), 19)
	}
	return ""
}
