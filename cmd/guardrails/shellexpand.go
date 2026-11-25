package main

// shellCtx resolves what the shell works out before it runs a command, as far as the text
// alone decides it, so that rules read the argv that runs rather than its spelling:
//
//   - variables the same command line assigned earlier (c=rm; "$c" ...), and $IFS while it
//     still holds its default;
//   - ANSI-C quoting ($'\x72\x6d') and command substitutions whose output is fixed by the
//     text ($(printf '\162\155'), `echo rm`; see staticOutput);
//   - brace expansion ({rm,-rf,~});
//   - word splitting of those values when unquoted (c="rm -rf"; $c ~);
//   - the positional parameters inside a function the line defines and then calls.
//
// Anything else (an inherited variable, a substitution that runs a real program) stays as
// written, "$HOME" or "$(which git)", which is what rules compare against. Control flow is
// ignored, so a value assigned in one branch counts in the other: that resolves more, never
// less. A nil shellCtx resolves nothing but text.

import (
	"encoding/base64"
	"path"
	"regexp"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

const (
	maxBraceWords = 64 // a larger brace expansion stays as written
	maxFuncDepth  = 4  // nested calls of the line's own functions
	maxFuncCalls  = 64 // function bodies walked again per parse, against a fan-out
)

// printfWidth matches a printf conversion with a width or precision of 4+ digits, which
// would build a huge string; such output is not worked out.
var printfWidth = regexp.MustCompile(`%[-+ #0'*]*[0-9]*\.?[0-9]{4,}`)

type shellCtx struct {
	vars  map[string]string       // NAME -> value, unresolved expansions in it kept as written
	funcs map[string]*syntax.Stmt // functions the line defined, by name
	pos   []string                // positional parameters inside a function call
	inFn  bool                    // pos is meaningful
	calls map[string]bool         // functions being expanded, against recursion
	walks *int                    // function bodies walked so far in this parse
}

func newShellCtx() *shellCtx {
	return &shellCtx{vars: map[string]string{}, funcs: map[string]*syntax.Stmt{}, calls: map[string]bool{}, walks: new(int)}
}

// call is the context inside a call of the line's function name with args.
func (c *shellCtx) call(name string, args []string) (*shellCtx, bool) {
	if c.calls[name] || len(c.calls) >= maxFuncDepth || *c.walks >= maxFuncCalls {
		return nil, false
	}
	*c.walks++
	calls := map[string]bool{name: true}
	for k := range c.calls {
		calls[k] = true
	}
	return &shellCtx{vars: c.vars, funcs: c.funcs, pos: args, inFn: true, calls: calls, walks: c.walks}, true
}

// assign records NAME=value assignments (plain or from export, declare, local, readonly).
func (c *shellCtx) assign(as []*syntax.Assign) {
	for _, a := range as {
		if a.Name != nil && a.Value != nil && !a.Naked && !a.Append && a.Array == nil && a.Index == nil {
			c.vars[a.Name.Value] = c.text(a.Value)
		}
	}
}

// text is the word as one string: wordText with what the context resolves substituted.
func (c *shellCtx) text(w *syntax.Word) string {
	s, _ := c.lit(w)
	return s
}

// lit is text, and whether every part of the word was resolved.
func (c *shellCtx) lit(w *syntax.Word) (string, bool) {
	var b fieldBuilder
	b.join, b.resolved = true, true
	for _, p := range w.Parts {
		c.part(&b, p, false)
	}
	return b.cur.String(), b.resolved
}

// fields are the words the shell passes for w: brace expansion, then expansions, with
// unquoted resolved values split on blanks and "$@" spliced.
func (c *shellCtx) fields(w *syntax.Word) []string {
	if c != nil {
		if words := braceWords(w); words != nil {
			var out []string
			for _, bw := range words {
				out = append(out, c.wordFields(bw)...)
			}
			return out
		}
	}
	return c.wordFields(w)
}

func (c *shellCtx) wordFields(w *syntax.Word) []string {
	b := fieldBuilder{resolved: true}
	for _, p := range w.Parts {
		c.part(&b, p, false)
	}
	b.flush()
	return b.fields
}

// braceWords is the brace expansion of w, or nil when it has none or too many words.
func braceWords(w *syntax.Word) []*syntax.Word {
	// SplitBraces rewrites the word it is given; expand a copy so the tree stays as parsed.
	cp := &syntax.Word{Parts: make([]syntax.WordPart, len(w.Parts))}
	for i, p := range w.Parts {
		if l, ok := p.(*syntax.Lit); ok {
			l2 := *l
			p = &l2
		}
		cp.Parts[i] = p
	}
	if !syntax.SplitBraces(cp) {
		return nil
	}
	var out []*syntax.Word
	for bw, err := range expand.BracesSeq(nil, cp) {
		if err != nil || len(out) == maxBraceWords {
			return nil
		}
		out = append(out, bw)
	}
	if len(out) < 2 {
		return nil
	}
	return out
}

// fieldBuilder accumulates the fields of one word.
type fieldBuilder struct {
	fields   []string
	cur      strings.Builder
	have     bool // cur is a field, even if empty ("" is one)
	join     bool // one string: never split
	resolved bool // no part was left as written
}

func (b *fieldBuilder) flush() {
	if b.have {
		b.fields = append(b.fields, b.cur.String())
	}
	b.cur.Reset()
	b.have = false
}

func (b *fieldBuilder) write(s string) {
	b.cur.WriteString(s)
	b.have = true
}

func isBlank(r rune) bool { return r == ' ' || r == '\t' || r == '\n' }

// split adds an unquoted expansion's value, cutting fields at blanks.
func (b *fieldBuilder) split(v string) {
	if b.join {
		b.cur.WriteString(v)
		return
	}
	if v != "" && isBlank(rune(v[0])) {
		b.flush()
	}
	for i, f := range strings.FieldsFunc(v, isBlank) {
		if i > 0 {
			b.flush()
		}
		b.write(f)
	}
	if v != "" && isBlank(rune(v[len(v)-1])) {
		b.flush()
	}
}

// splice adds "$@": one field per argument.
func (b *fieldBuilder) splice(args []string) {
	if b.join {
		b.cur.WriteString(strings.Join(args, " "))
		return
	}
	for i, a := range args {
		if i > 0 {
			b.flush()
		}
		b.write(a)
	}
}

func (c *shellCtx) part(b *fieldBuilder, p syntax.WordPart, quoted bool) {
	switch p := p.(type) {
	case *syntax.Lit:
		var s strings.Builder
		unescapeLit(&s, p.Value, quoted)
		b.write(s.String())
		return
	case *syntax.SglQuoted:
		v := p.Value
		if p.Dollar && c != nil {
			v, _, _ = expand.Format(nil, v, nil)
			v, _, _ = strings.Cut(v, "\x00")
		}
		b.write(v)
		return
	case *syntax.DblQuoted:
		b.have = true
		for _, q := range p.Parts {
			c.part(b, q, true)
		}
		return
	case *syntax.ParamExp:
		if c != nil && c.inFn && c.positional(b, p, quoted) {
			return
		}
		if v, ok := c.lookup(p); ok {
			if quoted {
				b.write(v)
			} else {
				b.split(v)
			}
			return
		}
	case *syntax.CmdSubst:
		if out, ok := c.staticOutput(p.Stmts); ok {
			out = strings.TrimRight(out, "\n")
			if quoted {
				b.write(out)
			} else {
				b.split(out)
			}
			return
		}
	}
	var buf strings.Builder
	syntax.NewPrinter().Print(&buf, p)
	b.write(buf.String())
	b.resolved = false
}

// simpleParam reports whether p is a bare $name or ${name}, with no operator.
func simpleParam(p *syntax.ParamExp) bool {
	return p.Param != nil && !p.Excl && !p.Length && !p.Width && p.Index == nil && p.Slice == nil &&
		p.Repl == nil && p.Exp == nil && p.Names == 0 && p.Flags == nil && p.NestedParam == nil && len(p.Modifiers) == 0
}

// lookup is the value of $name or ${name} when the line assigned it, and of an unassigned
// $IFS (its default, so an unquoted ${IFS} separates words).
func (c *shellCtx) lookup(p *syntax.ParamExp) (string, bool) {
	if c == nil || !simpleParam(p) {
		return "", false
	}
	v, ok := c.vars[p.Param.Value]
	if !ok && p.Param.Value == "IFS" {
		return " \t\n", true
	}
	return v, ok
}

// positional resolves $@ $* $# and $1..$9 inside a call of the line's own function.
func (c *shellCtx) positional(b *fieldBuilder, p *syntax.ParamExp, quoted bool) bool {
	if !simpleParam(p) {
		return false
	}
	switch name := p.Param.Value; {
	case name == "@" || name == "*" && !quoted:
		b.splice(c.pos)
	case name == "*":
		b.write(strings.Join(c.pos, " "))
	case name == "#":
		b.write(strconv.Itoa(len(c.pos)))
	case len(name) == 1 && name[0] >= '1' && name[0] <= '9':
		if i := int(name[0] - '1'); i < len(c.pos) {
			if quoted {
				b.write(c.pos[i])
			} else {
				b.split(c.pos[i])
			}
		} else if quoted {
			b.have = true
		}
	default:
		return false
	}
	return true
}

// staticOutput is what stmts write to stdout when the text alone fixes it: echo, printf,
// cat and base64 -d of a heredoc, here-string or pipe, and pipelines of those, with every
// word resolved. ok is false for anything else, a redirect of stdout included.
func (c *shellCtx) staticOutput(stmts []*syntax.Stmt) (string, bool) {
	if c == nil || len(stmts) != 1 {
		return "", false
	}
	return c.stmtOutput(stmts[0], nil)
}

func (c *shellCtx) stmtOutput(s *syntax.Stmt, stdin *string) (string, bool) {
	if s.Negated || s.Background || s.Coprocess {
		return "", false
	}
	for _, r := range s.Redirs {
		switch r.Op {
		case syntax.Hdoc, syntax.DashHdoc:
			if r.Hdoc == nil {
				return "", false
			}
			v, ok := c.lit(r.Hdoc)
			if !ok {
				return "", false
			}
			stdin = &v
		case syntax.WordHdoc:
			v, ok := c.lit(r.Word)
			if !ok {
				return "", false
			}
			v += "\n"
			stdin = &v
		default:
			return "", false
		}
	}
	switch cmd := s.Cmd.(type) {
	case *syntax.BinaryCmd:
		if cmd.Op != syntax.Pipe {
			return "", false
		}
		left, ok := c.stmtOutput(cmd.X, stdin)
		if !ok {
			return "", false
		}
		return c.stmtOutput(cmd.Y, &left)
	case *syntax.CallExpr:
		if len(cmd.Assigns) > 0 || len(cmd.Args) == 0 {
			return "", false
		}
		var args []string
		for _, w := range cmd.Args {
			if _, ok := c.lit(w); !ok {
				return "", false
			}
			args = append(args, c.fields(w)...)
		}
		if len(args) == 0 {
			return "", false
		}
		return builtinOutput(args, stdin)
	}
	return "", false
}

// builtinOutput runs the few output-only commands staticOutput knows.
func builtinOutput(args []string, stdin *string) (string, bool) {
	name, rest := path.Base(args[0]), args[1:]
	switch name {
	case "echo":
		newline, escapes := true, false
		for len(rest) > 0 && len(rest[0]) > 1 && rest[0][0] == '-' && strings.Trim(rest[0][1:], "neE") == "" {
			newline = newline && !strings.Contains(rest[0], "n")
			escapes = strings.Contains(rest[0], "e") || escapes && !strings.Contains(rest[0], "E")
			rest = rest[1:]
		}
		out := strings.Join(rest, " ")
		if escapes {
			out, _, _ = expand.Format(nil, strings.ReplaceAll(out, "%", "%%"), nil)
		}
		if newline {
			out += "\n"
		}
		return out, true
	case "printf":
		if len(rest) > 0 && rest[0] == "--" {
			rest = rest[1:]
		}
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") || printfWidth.MatchString(rest[0]) {
			return "", false
		}
		format, fargs := rest[0], rest[1:]
		var out strings.Builder
		for {
			s, n, err := expand.Format(nil, format, fargs)
			if err != nil {
				return "", false
			}
			out.WriteString(s)
			if n == 0 || n >= len(fargs) {
				break
			}
			fargs = fargs[n:]
		}
		return out.String(), true
	case "cat":
		if stdin == nil || len(rest) > 1 || len(rest) == 1 && rest[0] != "-" {
			return "", false
		}
		return *stdin, true
	case "base64":
		if stdin == nil || len(rest) != 1 || rest[0] != "-d" && rest[0] != "--decode" && rest[0] != "-D" {
			return "", false
		}
		data := strings.Map(func(r rune) rune {
			if isBlank(r) || r == '\r' {
				return -1
			}
			return r
		}, *stdin)
		dec, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return "", false
		}
		return string(dec), true
	}
	return "", false
}
