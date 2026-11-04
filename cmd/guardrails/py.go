package main

// Python-compatible text handling for the handoff writer port. Its prompts were measured as
// ~/.claude/hooks/handoff_writer.py built them, so each helper here reproduces one Python
// behavior: open() decoding and newlines, json.loads key order, code-point slicing,
// str.strip's whitespace, str()/repr()/json.dumps formatting, and re's Unicode classes.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dlclark/regexp2"
)

// obj is a JSON object with its keys in source order, as json.loads builds a dict: a
// repeated key keeps its first position and takes the last value.
type obj struct {
	keys []string
	vals map[string]any
}

func (o *obj) get(k string) any {
	if o == nil {
		return nil
	}
	return o.vals[k]
}

// lookup is dict.get(k, default) when the default matters: ok is false if k is absent.
func (o *obj) lookup(k string) (v any, ok bool) {
	if o == nil {
		return nil, false
	}
	v, ok = o.vals[k]
	return v, ok
}

func (o *obj) getObj(k string) *obj { v, _ := o.get(k).(*obj); return v }

// getStr is o.get(k) when only a string value is usable; anything else gives "".
func (o *obj) getStr(k string) string { v, _ := o.get(k).(string); return v }

// decodeJSON parses one JSON text as json.loads does, keeping key order and number literals.
func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("extra data")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := t.(json.Delim)
	if !ok {
		return t, nil // string, json.Number, bool or nil
	}
	switch d {
	case '{':
		o := &obj{vals: map[string]any{}}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			k, _ := kt.(string)
			v, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			if _, seen := o.vals[k]; !seen {
				o.keys = append(o.keys, k)
			}
			o.vals[k] = v
		}
		_, err = dec.Token()
		return o, err
	case '[':
		arr := []any{}
		for dec.More() {
			v, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		_, err = dec.Token()
		return arr, err
	}
	return nil, errors.New("unexpected delimiter")
}

// readPyText reads a file as Python's open(path[, errors="replace"]).read() does: UTF-8,
// invalid bytes replaced (replace) or an error, then \r\n and \r turned into \n.
func readPyText(path string, replace bool) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := string(data)
	if !utf8.Valid(data) {
		if !replace {
			return "", fmt.Errorf("%s: invalid UTF-8", path)
		}
		s = pyReplaceInvalid(data)
	}
	if strings.Contains(s, "\r") {
		s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	}
	return s, nil
}

// pyReplaceInvalid decodes UTF-8 like Python's errors="replace": one U+FFFD per maximal
// invalid subpart (a truncated sequence is one U+FFFD, not one per byte as in Go).
func pyReplaceInvalid(b []byte) string {
	var out strings.Builder
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r != utf8.RuneError || size > 1 {
			out.WriteRune(r)
			i += size
			continue
		}
		// Ranges of the second byte for each lead byte (Unicode Table 3-7); later bytes 80..BF.
		lo, hi, n := byte(0x80), byte(0xBF), 0
		switch c := b[i]; {
		case c >= 0xC2 && c <= 0xDF:
			n = 1
		case c == 0xE0:
			lo, n = 0xA0, 2
		case c == 0xED:
			hi, n = 0x9F, 2
		case c >= 0xE1 && c <= 0xEF:
			n = 2
		case c == 0xF0:
			lo, n = 0x90, 3
		case c >= 0xF1 && c <= 0xF3:
			n = 3
		case c == 0xF4:
			hi, n = 0x8F, 3
		}
		j := i + 1
		for k := 0; k < n && j < len(b); k++ {
			if b[j] < lo || b[j] > hi {
				break
			}
			lo, hi = 0x80, 0xBF
			j++
		}
		out.WriteRune(utf8.RuneError)
		i = j
	}
	return out.String()
}

// truthy is Python's bool() of a json.loads value.
func truthy(v any) bool {
	switch v := v.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case json.Number:
		f, _ := strconv.ParseFloat(string(v), 64)
		return f != 0
	case []any:
		return len(v) > 0
	case *obj:
		return v != nil && len(v.keys) > 0
	}
	return false
}

func isPySpace(r rune) bool { return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f }

// pyStrip is str.strip(): Python's whitespace also has \x1c-\x1f, which Go's lacks.
func pyStrip(s string) string { return strings.TrimFunc(s, isPySpace) }

// cut is s[:n] on code points.
func cut(s string, n int) string {
	i := 0
	for j := range s {
		if i == n {
			return s[:j]
		}
		i++
	}
	return s
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// pyLower is str.lower() except the context-dependent final sigma (Python lowers a final
// capital sigma to ς, this to σ); only the verify check uses it.
func pyLower(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "İ", "i̇"))
}

// pyStr is str() of a json.loads value.
func pyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pyRepr(v)
}

func pyRepr(v any) string {
	switch v := v.(type) {
	case nil:
		return "None"
	case bool:
		if v {
			return "True"
		}
		return "False"
	case string:
		return pyReprStr(v)
	case json.Number:
		return pyNum(v, false)
	case []any:
		parts := make([]string, len(v))
		for i, x := range v {
			parts[i] = pyRepr(x)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *obj:
		parts := make([]string, len(v.keys))
		for i, k := range v.keys {
			parts[i] = pyReprStr(k) + ": " + pyRepr(v.vals[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return ""
}

func pyReprStr(s string) string {
	quote := '\''
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteRune(quote)
	for _, r := range s {
		switch {
		case r == quote || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < ' ' || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x7f || unicode.IsPrint(r):
			b.WriteRune(r)
		case r <= 0xff:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r <= 0xffff:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteRune(quote)
	return b.String()
}

// pyNum formats a JSON number literal as Python prints what json.loads made of it: an int
// literal stays exact, anything else is a float in repr style (json.dumps spells inf as
// Infinity).
func pyNum(n json.Number, forJSON bool) string {
	lit := string(n)
	if !strings.ContainsAny(lit, ".eE") {
		if strings.Trim(lit, "-0") == "" {
			return "0"
		}
		return lit
	}
	f, _ := strconv.ParseFloat(lit, 64)
	if math.IsInf(f, 0) {
		s := "inf"
		if forJSON {
			s = "Infinity"
		}
		if f < 0 {
			s = "-" + s
		}
		return s
	}
	e := strconv.FormatFloat(f, 'e', -1, 64)
	exp, _ := strconv.Atoi(e[strings.IndexByte(e, 'e')+1:])
	if exp < -4 || exp >= 16 {
		return e
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// pyDumps is json.dumps(v) with its defaults: ", " and ": " separators, ensure_ascii.
func pyDumps(v any) string {
	switch v := v.(type) {
	case nil:
		return "null"
	case bool:
		if v {
			return "true"
		}
		return "false"
	case string:
		return pyDumpsStr(v)
	case json.Number:
		return pyNum(v, true)
	case []any:
		parts := make([]string, len(v))
		for i, x := range v {
			parts[i] = pyDumps(x)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *obj:
		parts := make([]string, len(v.keys))
		for i, k := range v.keys {
			parts[i] = pyDumpsStr(k) + ": " + pyDumps(v.vals[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return ""
}

func pyDumpsStr(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r >= ' ' && r <= '~':
				b.WriteRune(r)
			case r > 0xffff:
				r -= 0x10000
				fmt.Fprintf(&b, `\u%04x\u%04x`, 0xd800|(r>>10)&0x3ff, 0xdc00|r&0x3ff)
			default:
				fmt.Fprintf(&b, `\u%04x`, r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Python's re, str patterns: \w is any letter, number or underscore, \b sits between \w and
// non-\w, \s adds \x1c-\x1f to .NET's set, \d is any decimal digit, \Z is the very end.
const pyWord = `\p{L}\p{N}_`

var pyBoundary = `(?:(?<=[` + pyWord + `])(?![` + pyWord + `])|(?<![` + pyWord + `])(?=[` + pyWord + `]))`

// pyRe compiles a Python re pattern (as handoff_writer.py, facts.py and verify.py write
// them) for regexp2, whose $, ^ and . already behave as Python's.
func pyRe(pattern string, opts regexp2.RegexOptions) *regexp2.Regexp {
	var b strings.Builder
	inClass := false
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		if c == '\\' && i+1 < len(pattern) {
			i++
			switch e := pattern[i]; {
			case e == 'w' && inClass:
				b.WriteString(pyWord)
			case e == 'w':
				b.WriteString("[" + pyWord + "]")
			case e == 'W' && !inClass:
				b.WriteString("[^" + pyWord + "]")
			case e == 'd':
				b.WriteString(`\p{Nd}`)
			case e == 's' && inClass:
				b.WriteString(`\s\x1c-\x1f`)
			case e == 's':
				b.WriteString(`[\s\x1c-\x1f]`)
			case e == 'S' && !inClass:
				b.WriteString(`[^\s\x1c-\x1f]`)
			case e == 'b' && !inClass:
				b.WriteString(pyBoundary)
			case e == 'Z' && !inClass:
				b.WriteString(`\z`)
			case inClass && (e == 'W' || e == 'S' || e == 'D'):
				panic("pyRe: negated class escape inside a class: " + pattern)
			default:
				b.WriteByte('\\')
				b.WriteByte(e)
			}
			continue
		}
		switch {
		case c == '[' && !inClass:
			inClass = true
			b.WriteByte(c)
			if i+1 < len(pattern) && pattern[i+1] == '^' {
				b.WriteByte('^')
				i++
			}
			if i+1 < len(pattern) && pattern[i+1] == ']' {
				b.WriteString(`\]`)
				i++
			}
			continue
		case c == ']' && inClass:
			inClass = false
		}
		b.WriteByte(c)
	}
	return regexp2.MustCompile(b.String(), opts)
}

// pyMatchRe is pyRe anchored at the start, for re.match.
func pyMatchRe(pattern string, opts regexp2.RegexOptions) *regexp2.Regexp {
	return pyRe(`\A(?:`+pattern+`)`, opts)
}

// search is re.search; nil when nothing matches.
func search(re *regexp2.Regexp, s string) *regexp2.Match {
	m, _ := re.FindStringMatch(s)
	return m
}

// findAll is re.finditer.
func findAll(re *regexp2.Regexp, s string) []*regexp2.Match {
	var out []*regexp2.Match
	for m, _ := re.FindStringMatch(s); m != nil; m, _ = re.FindNextMatch(m) {
		out = append(out, m)
	}
	return out
}

// group is m.group(n), with "" for a group that did not take part.
func group(m *regexp2.Match, n int) string { return m.GroupByNumber(n).String() }

// sub is re.sub with a function replacement.
func sub(re *regexp2.Regexp, s string, rep func(m regexp2.Match) string) string {
	out, _ := re.ReplaceFunc(s, rep, -1, -1)
	return out
}

func subStr(re *regexp2.Regexp, s, rep string) string {
	return sub(re, s, func(regexp2.Match) string { return rep })
}

// pySection is the body of the markdown section headed "<hashes> <title>", as
// handoff_writer.section (hashes "##", strip=true) and verify.section ("###") take it.
func pySection(text, hashes, title string, strip bool, titleRest bool) string {
	rest := ""
	if titleRest {
		rest = ".*?"
	}
	re := pyRe(`^`+hashes+` `+regexp2.Escape(title)+rest+`\n(.*?)(?=^`+hashes+` |\Z)`, regexp2.Multiline|regexp2.Singleline)
	m := search(re, text)
	if m == nil {
		return ""
	}
	if strip {
		return pyStrip(group(m, 1))
	}
	return group(m, 1)
}
