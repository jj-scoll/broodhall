package main

// parseShell reads a Bash command with mvdan.cc/sh and returns every simple command in it:
// those after && ; | & and newlines, inside ( ), { }, $( ), backticks and process
// substitutions, and the ones a shell would run from a `bash -c '...'`, eval, ssh or
// heredoc/here-string handed to a shell. Quoted text, echo/grep arguments and heredoc bodies
// that feed a file or an interpreter are data, never commands. Rules read these argv lists
// instead of grepping the text.

import (
	"path"
	"strings"

	"github.com/jj-scoll/broodhall/internal/container"
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
	// Env holds the NAME=value assignments the command runs with: VAR=x prefixes, those
	// dropped from env/sudo, and the arguments of export.
	Env []string
}

const maxShellDepth = 5

// parseShell reports ok=false for text mvdan cannot parse, including a nested -c script,
// so callers can fall back to the legacy substring decision.
func parseShell(cmd string) ([]simpleCmd, bool) { return parseShellDepth(cmd, 0, nil) }

// parseShellDepth parses cmd, which runs inside a container on filesystem ms (nil on the
// host); the paths of its commands come back as host paths.
func parseShellDepth(cmd string, depth int, ms *container.MountSet) ([]simpleCmd, bool) {
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
	// command a container run|create|exec starts, and the commands find -exec runs (its bind mounts joining ms).
	var descend func(words, feeds []string, ms *container.MountSet)
	descend = func(words, feeds []string, ms *container.MountSet) {
		if len(words) > 0 && path.Base(words[0]) == "find" {
			for _, ea := range execArgs(words) {
				ic := mapCmd(simpleCmd{Words: peelWrappers(ea)}, ms)
				out = append(out, ic)
				descend(ic.Words, nil, ms)
			}
		}
		for _, script := range nestedScripts(words, feeds) {
			inner, innerOK := parseShellDepth(script.text, depth+1, ms)
			if !innerOK && !script.lenient {
				ok = false
			}
			out = append(out, inner...)
		}
		if inner, mounts := container.Cmd(words); inner != nil {
			ms = ms.Within(mounts)
			ic := mapCmd(simpleCmd{Words: peelWrappers(inner)}, ms)
			out = append(out, ic)
			descend(ic.Words, nil, ms)
		}
	}
	// walk adds the commands under node, resolving words with what sx knows (shellexpand.go);
	// a call of a function the line defined walks its body again with the call's arguments.
	var walk func(node syntax.Node, sx *shellCtx)
	walk = func(node syntax.Node, sx *shellCtx) {
		syntax.Walk(node, func(n syntax.Node) bool {
			switch n := n.(type) {
			case *syntax.FuncDecl:
				if selfPipeBomb(n) {
					out = append(out, simpleCmd{ForkBomb: true})
				}
				if n.Name != nil && n.Body != nil {
					sx.funcs[n.Name.Value] = n.Body
				}
			case *syntax.Stmt:
				sc := simpleCmd{}
				var ws []string
				if call, isCall := n.Cmd.(*syntax.CallExpr); isCall {
					for _, w := range call.Args {
						ws = append(ws, sx.fields(w)...)
					}
					sc.Words, sc.Env = peelWrappersEnv(ws)
					for _, as := range call.Assigns {
						if as.Name != nil && as.Value != nil && len(call.Args) > 0 {
							sc.Env = append(sc.Env, as.Name.Value+"="+sx.text(as.Value))
						}
					}
					if len(call.Args) == 0 {
						sx.assign(call.Assigns)
					}
				}
				if decl, isDecl := n.Cmd.(*syntax.DeclClause); isDecl && decl.Variant != nil {
					if decl.Variant.Value == "export" {
						sc.Words = []string{"export"}
						for _, as := range decl.Args {
							if as.Name != nil && as.Value != nil {
								sc.Env = append(sc.Env, as.Name.Value+"="+sx.text(as.Value))
							}
						}
					}
					sx.assign(decl.Args)
				}
				var feeds []string // heredoc / here-string bodies
				for _, r := range n.Redirs {
					if r.Word == nil {
						continue
					}
					sc.Redirs = append(sc.Redirs, astRedir{r.Op.String(), sx.text(r.Word)})
					switch r.Op {
					case syntax.Hdoc, syntax.DashHdoc:
						if r.Hdoc != nil {
							feeds = append(feeds, wordText(r.Hdoc))
						}
					case syntax.WordHdoc:
						feeds = append(feeds, sx.text(r.Word))
					}
				}
				sc = mapCmd(sc, ms)
				out = append(out, sc)
				descend(sc.Words, feeds, ms)
				if len(ws) > 0 && sx.funcs[ws[0]] != nil {
					if inner, ok := sx.call(ws[0], ws[1:]); ok {
						walk(sx.funcs[ws[0]], inner)
					}
				}
			}
			return true
		})
	}
	walk(f, newShellCtx())
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
		if script := dashC(args); script != "" {
			out = append(out, nestedScript{text: script})
		}
		for _, f := range feeds {
			out = append(out, nestedScript{text: f})
		}
	case "su", "script", "flock":
		if script := dashC(args); script != "" {
			out = append(out, nestedScript{text: script})
		}
	case "watch":
		if script := watchScript(args); script != "" {
			out = append(out, nestedScript{text: script})
		}
	case "trap":
		// trap 'handler' SIGNAL...: a handler of "-" resets the signals.
		if len(args) >= 2 && !strings.HasPrefix(args[0], "-") {
			out = append(out, nestedScript{text: args[0]})
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
func wordText(w *syntax.Word) string { return (*shellCtx)(nil).text(w) }

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
	if fn.Name == nil || fn.Body == nil { // "()0" parses as a FuncDecl with no name
		return false
	}
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

// mapCmd maps the words and redirect targets of c, which runs on m, to host paths.
func mapCmd(c simpleCmd, m *container.MountSet) simpleCmd {
	if m == nil {
		return c
	}
	words := make([]string, len(c.Words))
	for i, w := range c.Words {
		words[i] = m.MapPath(w)
	}
	redirs := make([]astRedir, len(c.Redirs))
	for i, r := range c.Redirs {
		redirs[i] = astRedir{r.Op, m.MapPath(r.Target)}
	}
	c.Words, c.Redirs = words, redirs
	return c
}
