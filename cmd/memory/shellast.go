package main

// parseShell reads a Bash command with mvdan.cc/sh and returns every simple command in it:
// those after && ; | & and newlines, inside ( ), { }, $( ), backticks and process
// substitutions, and the ones a shell would run from a `bash -c '...'`, eval, ssh or
// heredoc/here-string handed to a shell. Quoted text, echo/grep arguments and heredoc bodies
// that feed a file or an interpreter are data, never commands. Rules read these argv lists
// instead of grepping the text.

import (
	"bytes"
	"path"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// astRedir is one redirection: its operator (">", ">>", "<", "<<", ...) and target word.
type astRedir struct{ Op, Target string }

// simpleCmd is one statement: argv with wrappers (sudo, env, VAR=x) peeled and parameter
// expansions left as written ("$HOME"), plus its redirections. Words is empty for a
// statement that only wraps others (a subshell, a pipeline's redirect group).
type simpleCmd struct {
	Words    []string
	Redirs   []astRedir
	ForkBomb bool // a function that pipes itself into itself in the background
}

const maxShellDepth = 5

// parseShell reports ok=false for text mvdan cannot parse, including a nested -c script,
// so callers can fall back to the legacy substring decision.
func parseShell(cmd string) ([]simpleCmd, bool) { return parseShellDepth(cmd, 0, nil) }

// parseShellDepth parses cmd, which runs inside a container on filesystem ms (nil on the
// host); the paths of its commands come back as host paths.
func parseShellDepth(cmd string, depth int, ms *mountSet) ([]simpleCmd, bool) {
	if depth > maxShellDepth {
		return nil, false
	}
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(cmd), "")
	if err != nil {
		return nil, false
	}
	var out []simpleCmd
	ok := true
	// descend adds what a command runs one level down: a script handed to a shell, and the
	// command a container run|create|exec starts (its bind mounts joining ms).
	var descend func(words, feeds []string, ms *mountSet)
	descend = func(words, feeds []string, ms *mountSet) {
		for _, script := range nestedScripts(words, feeds) {
			inner, innerOK := parseShellDepth(script.text, depth+1, ms)
			if !innerOK && !script.lenient {
				ok = false
			}
			out = append(out, inner...)
		}
		if inner, mounts := containerCmd(words); inner != nil {
			ms = ms.within(mounts)
			ic := mapCmd(simpleCmd{Words: peelWrappers(inner)}, ms)
			out = append(out, ic)
			descend(ic.Words, nil, ms)
		}
	}
	syntax.Walk(f, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.FuncDecl:
			if selfPipeBomb(n) {
				out = append(out, simpleCmd{ForkBomb: true})
			}
		case *syntax.Stmt:
			sc := simpleCmd{}
			if call, isCall := n.Cmd.(*syntax.CallExpr); isCall {
				ws := make([]string, len(call.Args))
				for i, w := range call.Args {
					ws[i] = wordText(w)
				}
				sc.Words = peelWrappers(ws)
			}
			var feeds []string // heredoc / here-string bodies
			for _, r := range n.Redirs {
				if r.Word == nil {
					continue
				}
				sc.Redirs = append(sc.Redirs, astRedir{r.Op.String(), wordText(r.Word)})
				switch r.Op {
				case syntax.Hdoc, syntax.DashHdoc:
					if r.Hdoc != nil {
						feeds = append(feeds, wordText(r.Hdoc))
					}
				case syntax.WordHdoc:
					feeds = append(feeds, wordText(r.Word))
				}
			}
			sc = mapCmd(sc, ms)
			out = append(out, sc)
			descend(sc.Words, feeds, ms)
		}
		return true
	})
	if !ok {
		return nil, false
	}
	return out, true
}

type nestedScript struct {
	text    string
	lenient bool // a failed parse is not a failure of the whole command (ssh argument guesses)
}

// nestedScripts lists the text a command hands to a shell: bash -c '...', eval, the
// remote command of ssh, and heredoc/here-string bodies fed to a shell or ssh.
func nestedScripts(w []string, feeds []string) []nestedScript {
	if len(w) == 0 {
		return nil
	}
	var out []nestedScript
	args := w[1:]
	switch path.Base(w[0]) {
	case "bash", "sh", "zsh", "dash", "ksh":
		for i, a := range args {
			if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c") && i+1 < len(args) {
				out = append(out, nestedScript{text: args[i+1]})
				break
			}
		}
		for _, f := range feeds {
			out = append(out, nestedScript{text: f})
		}
	case "eval":
		out = append(out, nestedScript{text: strings.Join(args, " ")})
	case "ssh":
		// Options and the host come first; try each tail as the remote command.
		for i := range args {
			out = append(out, nestedScript{text: strings.Join(args[i:], " "), lenient: true})
		}
		for _, f := range feeds {
			out = append(out, nestedScript{text: f})
		}
	}
	return out
}

// wordText is the word as the shell would pass it, except that expansions stay literal
// ("$HOME", "$(cmd)"), which is what rules compare against.
func wordText(w *syntax.Word) string {
	var b strings.Builder
	for _, p := range w.Parts {
		partText(&b, p, false)
	}
	return b.String()
}

func partText(b *strings.Builder, p syntax.WordPart, dquote bool) {
	switch p := p.(type) {
	case *syntax.Lit:
		unescapeLit(b, p.Value, dquote)
	case *syntax.SglQuoted:
		b.WriteString(p.Value)
	case *syntax.DblQuoted:
		for _, q := range p.Parts {
			partText(b, q, true)
		}
	default:
		var buf bytes.Buffer
		syntax.NewPrinter().Print(&buf, p)
		b.Write(buf.Bytes())
	}
}

// unescapeLit drops the backslash of an escaped character; inside double quotes only \" \\
// \$ and \` are escapes.
func unescapeLit(b *strings.Builder, s string, dquote bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && (!dquote || strings.IndexByte("\"\\$`", s[i+1]) >= 0) {
			i++
		}
		b.WriteByte(s[i])
	}
}

// selfPipeBomb matches the fork bomb shape: f() { f | f & }.
func selfPipeBomb(fn *syntax.FuncDecl) bool {
	name := fn.Name.Value
	calls := func(s *syntax.Stmt) bool {
		c, ok := s.Cmd.(*syntax.CallExpr)
		return ok && len(c.Args) > 0 && wordText(c.Args[0]) == name
	}
	found := false
	syntax.Walk(fn.Body, func(n syntax.Node) bool {
		if s, ok := n.(*syntax.Stmt); ok && s.Background {
			if b, ok := s.Cmd.(*syntax.BinaryCmd); ok && b.Op == syntax.Pipe && calls(b.X) && calls(b.Y) {
				found = true
			}
		}
		return !found
	})
	return found
}
