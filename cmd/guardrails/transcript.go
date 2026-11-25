package main

// Transcript reading shared by the handoff writer's digest and facts block: ports of
// ~/.claude/hooks/sessiondb/ingest.py text_of and typed_prompt, and of the read loop both
// handoff_writer.digest and facts.build open with.

import (
	"fmt"
	"strings"

	"github.com/jj-scoll/broodhall/internal/pytext"
)

// jsonLines is `for line in open(path, errors="replace")` plus json.loads: every line that
// parses, whatever it parses to.
func jsonLines(path string) ([]any, error) {
	text, err := pytext.ReadText(path, true)
	if err != nil {
		return nil, err
	}
	var out []any
	for _, line := range strings.SplitAfter(text, "\n") {
		if v, err := pytext.DecodeJSON([]byte(line)); err == nil {
			out = append(out, v)
		}
	}
	return out, nil
}

// transcriptLines is jsonLines for callers that go on to call d.get(...) on each line: one that
// parses to something other than an object stops it, as d.get raises in Python.
func transcriptLines(path string) ([]*pytext.Obj, error) {
	values, err := jsonLines(path)
	if err != nil {
		return nil, err
	}
	var out []*pytext.Obj
	for _, v := range values {
		d, ok := v.(*pytext.Obj)
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
			o, ok := b.(*pytext.Obj)
			if !ok {
				parts = append(parts, pytext.Str(b))
				continue
			}
			if t := o.Get("type"); t == nil || t == "text" {
				parts = append(parts, getStr(o, "text", ""))
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// typedPrompt is ingest.typed_prompt: the text of a prompt the person typed, else "".
func typedPrompt(d, msg *pytext.Obj) string {
	if pytext.Truthy(d.Get("isMeta")) {
		return ""
	}
	content := msg.Get("content")
	if list, ok := content.([]any); ok {
		for _, b := range list {
			if o, ok := b.(*pytext.Obj); ok && o.Get("type") == "tool_result" {
				return ""
			}
		}
	}
	text := pytext.Strip(textOf(content))
	if origin, ok := d.Get("origin").(*pytext.Obj); ok {
		if origin.Get("kind") == "human" {
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
func getStr(o *pytext.Obj, k, def string) string {
	if v, ok := o.Lookup(k); ok {
		return pytext.Str(v)
	}
	return def
}

// stamp is (d.get("timestamp") or "")[:19].
func stamp(d *pytext.Obj) string {
	if ts := d.Get("timestamp"); pytext.Truthy(ts) {
		return pytext.Cut(pytext.Str(ts), 19)
	}
	return ""
}
