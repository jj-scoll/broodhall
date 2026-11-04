package main

// Secret-file guard (pre-tool-use). The protected files are secretPathsDefault plus the
// lines of ~/.config/claude-tools/secret-paths (a path or glob per line, ~ allowed, #
// comments). The list adds to the defaults and is protected itself. Claude sees the
// contents masked:
//
//   - Bash: a plain reader of a protected file (cat, head, tail, grep, rg, sed -n 'Np',
//     jq ., yq .), or a recursive grep/rg of a directory holding one, is rewritten to
//     `guardrails redact --secrets -- <reader ...>`. Only flags that print whole lines
//     pass, because key-name masking needs the key on the line. Metadata commands (ls,
//     stat, test, wc, ...) pass. Any other command naming a protected file asks: base64,
//     xxd, cp or python would get past the mask. So does a protected path anywhere else in
//     the text (bash -c, python -c, X=path), and a secret-shaped value in the command.
//   - Read: blocked, pointing at cat. Edit/Write: ask.
//
// Like the PII rules guard this is a speed bump, not a wall: the model runs as the user, and a
// script written earlier or a path built from variables gets past it.

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

var secretPathsDefault = []string{
	"~/.config/mark.toml",
	"~/.claude/.credentials.json", "~/.claude-work/.credentials.json",
	"~/.aws/credentials", "~/.aws/sso/cache/*.json", "~/.aws/cli/cache/*.json",
	"~/.ssh/id_rsa", "~/.ssh/id_ecdsa", "~/.ssh/id_ed25519", "~/.ssh/id_ecdsa_sk", "~/.ssh/id_ed25519_sk",
	"~/.netrc", "~/.pgpass", "~/.docker/config.json", "~/.config/gh/hosts.yml",
	"~/.local/state/claude-tools/pii-key",
}

const secretMaskNote = "pre-tool-use masked this read of a protected secret file (`guardrails redact --secrets`): " +
	"token shapes show as [kind:tag] and values under secret-looking keys as [secret:tag]; equal values share a tag. " +
	"The real values stay in the file: run the tool that uses it rather than copying values."

var (
	secretMeta    = map[string]bool{"ls": true, "stat": true, "test": true, "[": true, "wc": true, "du": true, "file": true, "realpath": true, "readlink": true, "dirname": true, "basename": true}
	sedPrintRE    = regexp.MustCompile(`^(?:\d+|\$|/[^/\n]*/)(?:,(?:\d+|\$|/[^/\n]*/))?p$`)
	headTailOK    = regexp.MustCompile(`^(?:-\d+|-n\d*|-q|-v|--lines(?:=.*)?|--quiet|--silent|--verbose)$`)
	jqFlagsOK     = map[string]bool{"-c": true, "-r": true, "-S": true, "-M": true, "-P": true, "--tab": true, "--compact-output": true, "--raw-output": true, "--sort-keys": true, "--monochrome-output": true, "--no-colors": true, "--prettyPrint": true}
	findRunsRE    = regexp.MustCompile(`^-(?:exec|execdir|ok|okdir|delete|fprint|fprint0|fprintf|fls)$`)
	secretGlobChr = "*?["
)

// secretPaths is every protected path pattern, absolute and cleaned.
func secretPaths(home, xdg string) []string {
	ps := append([]string{}, secretPathsDefault...)
	if rules := piiRulesPathFor(xdg, home); rules != "" {
		list := filepath.Join(filepath.Dir(rules), "secret-paths")
		ps = append(ps, list)
		if b, err := os.ReadFile(list); err == nil {
			for _, l := range strings.Split(string(b), "\n") {
				if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
					ps = append(ps, l)
				}
			}
		}
	}
	var out []string
	for _, p := range ps {
		if p = filepath.Clean(expandHome(p, home)); filepath.IsAbs(p) { // a relative line names no one file
			out = append(out, p)
		}
	}
	return out
}

type secretGuard struct {
	home, cwd string
	paths     []string // as written, then with symlinks resolved
}

func newSecretGuard(home, xdg, cwd string) *secretGuard {
	g := &secretGuard{home: home, cwd: cwd}
	ps := secretPaths(home, xdg)
	for _, p := range ps {
		dir, base := filepath.Split(p)
		g.paths = append(g.paths, filepath.Join(resolveForCompare(dir, "/"), base))
	}
	g.paths = append(ps, g.paths...)
	return g
}

// files is what a path word may name: ~ and $HOME expanded, made absolute, globs expanded
// (dot files only for a dotted pattern, as the shell does). Nil when it holds other
// expansions.
func (g *secretGuard) files(w string) []string {
	for _, v := range []string{"${HOME}", "$HOME"} {
		if strings.HasPrefix(w, v) {
			w = g.home + w[len(v):]
		}
	}
	w = expandHome(w, g.home)
	if w == "" || strings.ContainsAny(w, "$`") {
		return nil
	}
	if !filepath.IsAbs(w) {
		w = filepath.Join(g.cwd, w)
	}
	w = filepath.Clean(w)
	if !strings.ContainsAny(w, secretGlobChr) {
		return []string{w}
	}
	ms, _ := filepath.Glob(w)
	out := []string{w}
	for _, m := range ms {
		if !strings.HasPrefix(filepath.Base(m), ".") || strings.HasPrefix(filepath.Base(w), ".") {
			out = append(out, m)
		}
	}
	return out
}

// protected reports whether word names a protected file; under reports whether it names a
// directory holding one.
func (g *secretGuard) protected(word string) (file, under bool) {
	for _, f := range g.files(word) {
		for _, c := range []string{f, resolveForCompare(f, "/")} {
			for _, p := range g.paths {
				ok, _ := filepath.Match(p, c)
				file = file || ok
				under = under || strings.HasPrefix(p, strings.TrimSuffix(c, "/")+"/")
			}
		}
	}
	return file, under
}

// readerOK reports whether a reader's flags keep each output line whole, key included.
func readerOK(name string, args []string) bool {
	var plain []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			plain = append(plain, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			plain = append(plain, a)
			continue
		}
		short := !strings.HasPrefix(a, "--")
		switch name {
		case "cat":
			if !short || strings.Trim(a[1:], "nbsu") != "" {
				return false
			}
		case "head", "tail":
			if !headTailOK.MatchString(a) {
				return false
			}
			if a == "-n" || a == "--lines" {
				i++
			}
		case "grep", "rg":
			bad := "of"
			if name == "rg" {
				bad = "ofrp"
			}
			for _, p := range []string{"--only-matching", "--file", "--color=always", "--colour=always", "--json", "--replace", "--pretty", "--pre"} {
				if !short && strings.HasPrefix(a, p) {
					return false
				}
			}
			if short && strings.ContainsAny(shortLetters(name, a), bad) {
				return false
			}
		case "sed":
			if a != "-n" && a != "--quiet" && a != "--silent" && a != "-E" && a != "-r" {
				return false
			}
		case "jq", "yq":
			if !jqFlagsOK[a] {
				return false
			}
		}
	}
	switch name {
	case "sed":
		return len(plain) > 0 && sedPrintRE.MatchString(plain[0]) && (slices.Contains(args, "-n") || slices.Contains(args, "--quiet") || slices.Contains(args, "--silent"))
	case "yq":
		if len(plain) > 0 && (plain[0] == "e" || plain[0] == "eval") {
			plain = plain[1:]
		}
		fallthrough
	case "jq":
		return len(plain) > 0 && plain[0] == "."
	}
	return true
}

// grepValueFlags are the short grep and rg flags that take a value: in a cluster, the
// letters after one are its value.
var grepValueFlags = map[string]string{"grep": "efmABCdD", "rg": "efgmMrtTjABCE"}

// shortLetters is the flags of a short cluster (-rn) up to and including the first one that
// takes a value, "" for anything else.
func shortLetters(name, a string) string {
	if !strings.HasPrefix(a, "-") || strings.HasPrefix(a, "--") {
		return ""
	}
	if i := strings.IndexAny(a[1:], grepValueFlags[name]); i >= 0 {
		return a[1 : i+2]
	}
	return a[1:]
}

// printsNoLines: grep or rg printing file names, counts or nothing (rg -L follows links).
func printsNoLines(name string, args []string) bool {
	quiet := "lcq"
	if name == "grep" {
		quiet = "lcqL"
	}
	return slices.ContainsFunc(args, func(a string) bool {
		return strings.ContainsAny(shortLetters(name, a), quiet) ||
			slices.Contains([]string{"--files-with-matches", "--files-without-match", "--count", "--count-matches", "--files", "--quiet", "--silent"}, a)
	})
}

var secretReaders = map[string]bool{"cat": true, "head": true, "tail": true, "grep": true, "rg": true, "sed": true, "jq": true, "yq": true}

// secretScan is the Bash analysis: wrap holds the offsets of readers to run under redact,
// ask the command that must be confirmed ("" when none).
type secretScan struct {
	wrap []int
	ask  string
}

func (g *secretGuard) scan(cmd string) secretScan {
	var s secretScan
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(cmd), "")
	if err != nil {
		if g.unaccounted(cmd, nil) {
			s.ask = "text"
		}
		return s
	}
	var covered [][2]int
	cover := func(n syntax.Node) { covered = append(covered, [2]int{int(n.Pos().Offset()), int(n.End().Offset())}) }
	askOnce := func(name string) {
		if s.ask == "" {
			s.ask = name
		}
	}
	// T=$(...): output captured into a variable is used, not shown.
	var captured [][2]int
	syntax.Walk(f, func(n syntax.Node) bool {
		if as, ok := n.(*syntax.Assign); ok && as.Value != nil {
			for _, p := range as.Value.Parts {
				if dq, ok := p.(*syntax.DblQuoted); ok && len(dq.Parts) == 1 {
					p = dq.Parts[0]
				}
				if cs, ok := p.(*syntax.CmdSubst); ok {
					captured = append(captured, [2]int{int(cs.Pos().Offset()), int(cs.End().Offset())})
				}
			}
		}
		return true
	})
	syntax.Walk(f, func(n syntax.Node) bool {
		st, ok := n.(*syntax.Stmt)
		if !ok {
			return true
		}
		var in, out bool
		shown := false // output goes to a file, not the capturing variable
		for _, r := range st.Redirs {
			if r.Word == nil {
				continue
			}
			switch r.Op {
			case syntax.RdrOut, syntax.AppOut, syntax.ClbOut, syntax.RdrAll, syntax.AppAll:
				shown = shown || wordText(r.Word) != "/dev/null"
			}
			if file, _ := g.protected(wordText(r.Word)); file {
				if r.Op == syntax.RdrIn {
					in = true
				} else {
					out = true
				}
			}
		}
		if !shown && slices.ContainsFunc(captured, func(c [2]int) bool {
			return int(st.Pos().Offset()) >= c[0] && int(st.End().Offset()) <= c[1]
		}) {
			cover(st)
			return true
		}
		ce, _ := st.Cmd.(*syntax.CallExpr)
		if ce == nil || len(ce.Args) == 0 {
			if in || out {
				askOnce("redirect")
			}
			return true
		}
		words := make([]string, len(ce.Args))
		for i, a := range ce.Args {
			words[i] = wordText(a)
		}
		name := path.Base(words[0])
		var file, under bool
		for _, w := range words[1:] {
			f, u := g.protected(w)
			file, under = file || f, under || u
		}
		recursive := name == "rg" || name == "grep" && slices.ContainsFunc(words[1:], func(a string) bool {
			return a == "--recursive" || a == "--dereference-recursive" || strings.ContainsAny(shortLetters("grep", a), "rR")
		})
		switch {
		case out:
			askOnce(name)
		case name == "cd" || name == "pushd":
			if fs := g.files(words[len(words)-1]); len(words) > 1 && len(fs) == 1 {
				g.cwd = fs[0]
			}
			cover(st)
		case secretMeta[name], (name == "grep" || name == "rg") && printsNoLines(name, words[1:]):
			cover(st)
		case file || in || under && recursive:
			if secretReaders[name] && readerOK(name, words[1:]) {
				s.wrap = append(s.wrap, int(ce.Args[0].Pos().Offset()))
				cover(st)
			} else {
				askOnce(name)
			}
		case under && name == "find" && slices.ContainsFunc(words[1:], findRunsRE.MatchString):
			askOnce(name)
		}
		return true
	})
	if s.ask == "" && g.unaccounted(cmd, covered) {
		s.ask = "text"
	}
	return s
}

// unaccounted reports whether cmd spells a protected path (as written, from ~ or $HOME;
// a glob up to its first wildcard) outside the covered byte ranges.
func (g *secretGuard) unaccounted(cmd string, covered [][2]int) bool {
	for _, p := range g.paths {
		forms := []string{p}
		if rest, ok := strings.CutPrefix(p, g.home+"/"); ok && g.home != "" {
			forms = append(forms, "~/"+rest, "$HOME/"+rest, "${HOME}/"+rest)
		}
		for _, n := range forms {
			exact := true
			if i := strings.IndexAny(n, secretGlobChr); i >= 0 {
				n, exact = n[:i], false
			}
			for off := 0; ; {
				i := strings.Index(cmd[off:], n)
				if i < 0 {
					break
				}
				at, end := off+i, off+i+len(n)
				off = at + 1
				if exact && end < len(cmd) && strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_.-", rune(cmd[end])) {
					continue
				}
				inside := false
				for _, c := range covered {
					inside = inside || at >= c[0] && end <= c[1]
				}
				if !inside {
					return true
				}
			}
		}
	}
	return false
}

func secretAsk(rule, why string) decision {
	return decision{Ask: true, Rule: rule, Msg: "ASK: " + why + " Confirm with the user before running it."}
}

// secretFileRule is the decide() half: ask, the mask note, or nothing.
func secretFileRule(env *guardEnv, cmd string) decision {
	if m := scanSecrets(cmd); len(m) > 0 {
		return secretAsk("secret-arg:"+strings.ToLower(strings.ReplaceAll(m[0].Pattern, " ", "-")),
			"the command holds a secret-shaped value ("+m[0].Pattern+" "+m[0].Redacted+"). Prefer a file or variable the tool reads itself.")
	}
	s := newSecretGuard(env.Home, env.XDGConfig, env.Cwd).scan(cmd)
	switch {
	case s.ask != "":
		return secretAsk("secret-file:"+s.ask, "it reads or writes a protected secret file in a way the mask cannot cover. "+
			"Plain cat, head, tail, grep, rg, sed -n 'Np' and jq . reads are masked; ls, stat, test and wc pass; "+
			`to use a value without showing it, capture it: T=$(sed -n 's/^password = "\(.*\)"$/\1/p' FILE).`)
	case len(s.wrap) > 0:
		return decision{Rule: "secret-file:mask", Context: secretMaskNote}
	}
	return decision{}
}

// secretRewrite runs every masked reader in cmd under `self redact --secrets --`.
func secretRewrite(env *guardEnv, cmd, self string) (string, bool) {
	s := newSecretGuard(env.Home, env.XDGConfig, env.Cwd).scan(cmd)
	if s.ask != "" || len(s.wrap) == 0 {
		return "", false
	}
	qself, err := syntax.Quote(self, syntax.LangBash)
	if err != nil {
		return "", false
	}
	sort.Sort(sort.Reverse(sort.IntSlice(s.wrap)))
	for _, at := range s.wrap {
		cmd = cmd[:at] + qself + " redact --secrets -- " + cmd[at:]
	}
	return cmd, true
}

// secretFileToolDecision is the non-Bash half: Read of a protected file is blocked, edits ask.
func secretFileToolDecision(tool string, input map[string]any, cwd, home, xdg string) (decision, bool) {
	p, _ := input["file_path"].(string)
	if p == "" {
		p, _ = input["notebook_path"].(string)
	}
	if p == "" {
		return decision{}, false
	}
	if file, _ := newSecretGuard(home, xdg, cwd).protected(p); !file {
		return decision{}, false
	}
	switch tool {
	case "Read":
		return decision{Exit: 2, Rule: "secret-file:Read", Msg: "BLOCKED: " + p + " is a protected secret file. Read it in Bash with `cat " + p +
			"`: pre-tool-use masks its values."}, true
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return secretAsk("secret-file:"+tool, p+" is a protected secret file."), true
	}
	return decision{}, false
}
