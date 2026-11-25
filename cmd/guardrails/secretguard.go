package main

// Secret-file guard (pre-tool-use). The protected files are secretPathsDefault, the Claude
// credentials in each config directory (claudeConfigDirs), the PII key, project files by name
// (.env, *.tfvars, *.pem, ... in any directory), plus the
// lines of ~/.config/guardrails/secret-paths and ~/.config/claude-tools/secret-paths (a path or
// glob per line, ~ allowed, # comments). Each list adds to the defaults and is protected
// itself. Claude sees the
// contents masked:
//
//   - Bash: a plain reader of a protected file (cat, head, tail, grep, rg, sed -n 'Np',
//     jq ., yq .), or a recursive grep/rg of a directory holding one, is rewritten to
//     `guardrails redact --secrets -- <reader ...>`. Only flags that print whole lines
//     pass, because key-name masking needs the key on the line. A recursive grep/rg/git grep
//     of a directory that holds a file protected by name only runs under `--by-file`, which
//     masks the lines whose file name is protected. Metadata commands (ls,
//     stat, test, wc, ...) pass. Any other command naming a protected file asks: base64,
//     xxd, cp or python would get past the mask. So does a protected path anywhere else in
//     the text (bash -c, python -c, X=path), and a secret-shaped value in the command.
//   - Read: blocked, pointing at cat. Edit/Write: ask.
//
// Like the PII rules guard this is a speed bump, not a wall: the model runs as the user, and a
// script written earlier or a path built from variables gets past it.

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jj-scoll/broodhall/cmd/guardrails/secretfiles"
	"mvdan.cc/sh/v3/syntax"
)

// secretPathsDefault is the fixed part of the list. secretPaths adds the Claude credentials in
// each config directory and the PII key under XDG_STATE_HOME. ~/.config/mark.toml is the
// config of mark (github.com/kovetskiy/mark, Markdown to Confluence), which holds an
// Atlassian API token in plain text. No broodhall code reads it; it stays as a well-known
// tool's credentials file, like gh's hosts.yml.
var secretPathsDefault = secretfiles.Paths

// secretNamesDefault are file names protected in any directory (filepath.Match on the base
// name), and secretNamesSkip the templates among them that hold no secret. Only the defaults
// use names: the lines of a secret-paths file stay absolute paths. The lists live in
// cmd/guardrails/secretfiles so the memory daemon shares them.
var (
	secretNamesDefault = secretfiles.Names
	secretNamesSkip    = secretfiles.Skip
)

// secretNamed reports whether a file's base name is one of secretNamesDefault.
func secretNamed(base string) bool { return secretfiles.Named(base) }

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
	for _, d := range claudeConfigDirs(home) {
		ps = append(ps, filepath.Join(d, ".credentials.json"))
	}
	if st := os.Getenv("XDG_STATE_HOME"); st != "" { // where redact keeps the key (stateBase)
		ps = append(ps, userPaths(st, "pii-key")...)
	}
	if base := configBase(xdg, home); base != "" {
		for _, list := range userPaths(base, "secret-paths") {
			ps = append(ps, list)
			if b, err := os.ReadFile(list); err == nil {
				for _, l := range strings.Split(string(b), "\n") {
					if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
						ps = append(ps, l)
					}
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
	self      string   // the guardrails binary whose wrapper counts as masked ("" for none)
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

// files is what a path word may name: ~, $HOME and $CLAUDE_CONFIG_DIR expanded, made absolute, globs expanded
// (dot files only for a dotted pattern, as the shell does). Nil when it holds other
// expansions.
func (g *secretGuard) files(w string) []string {
	w = expandHome(expandPathVars(w, g.home), g.home)
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
				// A ~/.claude* dir that does not exist yet is not in g.paths.
				file = file || secretNamed(filepath.Base(c))
				file = file || filepath.Base(c) == ".credentials.json" && isClaudeConfigDir(filepath.Dir(c), g.home)
				under = under || strings.HasPrefix(p, strings.TrimSuffix(c, "/")+"/")
			}
		}
	}
	return file, under
}

// ownWrapper reports whether words are the command secretRewrite emits: this very binary,
// `redact --secrets --`, then a reader whose flags the mask accepts. Another binary, another
// flag or a non-reader is judged as if the wrapper were not there. A pipeline stage after the
// wrapper, or a command chained to it, is a statement of its own and is scanned on its own.
func (g *secretGuard) ownWrapper(words []string) bool {
	if g.self == "" || len(words) < 5 || words[0] != g.self || words[1] != "redact" || words[2] != "--secrets" || words[3] != "--" {
		return false
	}
	name := path.Base(words[4])
	return secretReaders[name] && readerOK(name, words[5:])
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

// The walk of searchHoldsNamed is bounded by entries and by wall time; past either it
// stops and reports "holds one" (fail closed: the search is masked). The budget is the
// pre-tool-use latency a recursive search may pay: 15ms is below what a person notices next
// to the shell and model round trip, and on a loaded machine it covers a few thousand
// entries, so a repo-sized tree is fully checked and a home-sized one fails closed.
// searchWalkNow is the clock, replaced in tests.
const searchWalkCap = 50000

var (
	searchWalkBudget = 15 * time.Millisecond
	searchWalkNow    = time.Now
	// searchWalkSkip are dependency, vendor and tool-cache directories: large, rarely hold
	// a project's own .env, and a hit there is a third party's file. A .env inside one is
	// not masked by name.
	searchWalkSkip = map[string]bool{".git": true, "node_modules": true, "vendor": true, ".direnv": true, ".terraform": true, ".venv": true}
)

// searchHoldsNamed reports whether a recursive search reaches a file protected by name
// (.env, *.tfvars, ...): a directory among its words, or the cwd when it names none (rg,
// git grep, grep -r), holds one. Only searches that can print such a file are wrapped, so
// an ordinary `grep -r` keeps its output (and rtk's compression of it) as typed.
func (g *secretGuard) searchHoldsNamed(words []string, gitGrep bool) bool {
	var roots []string
	for _, w := range words {
		if gitGrep || strings.HasPrefix(w, "-") {
			continue
		}
		for _, f := range g.files(w) {
			if fi, err := os.Stat(f); err == nil && fi.IsDir() {
				roots = append(roots, f)
			}
		}
	}
	if len(roots) == 0 {
		roots = []string{g.cwd}
	}
	for _, r := range roots {
		if searchWalkMemo[r] || !searchWalkMemoHas(r) && g.walkHoldsNamed(r) {
			return true
		}
	}
	return false
}

// searchWalkMemo remembers each root's walk for the life of the process: pre-tool-use scans
// a command twice (the rule, then the rewrite) and the answer cannot change in between, so
// the budget is paid once per call. Tests reset it.
var searchWalkMemo = map[string]bool{}

func searchWalkMemoHas(r string) bool { _, ok := searchWalkMemo[r]; return ok }

// walkHoldsNamed walks root within the entry and time budget and memoizes the answer.
func (g *secretGuard) walkHoldsNamed(root string) bool {
	n, found := 0, false
	deadline := searchWalkNow().Add(searchWalkBudget)
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if n++; n > searchWalkCap || secretNamed(d.Name()) && !d.IsDir() || n%16 == 0 && searchWalkNow().After(deadline) {
			found = true
			return filepath.SkipAll
		}
		if d.IsDir() && searchWalkSkip[d.Name()] {
			return filepath.SkipDir
		}
		return nil
	})
	searchWalkMemo[root] = found
	return found
}

// namesFiles reports whether a grep or rg prints the file name on every line (path:text), as
// by-file masking reads it: not with -h/--no-filename/-I, rg --heading (the name is its own
// line), or -Z/-0/--null (a NUL replaces the colon).
func namesFiles(name string, args []string) bool {
	for _, a := range args {
		if a == "--" {
			break
		}
		if slices.Contains([]string{"--no-filename", "--heading", "--null", "--null-data"}, a) {
			return false
		}
		bad := "hZz"
		if name == "rg" {
			bad = "I0"
		}
		if strings.ContainsAny(shortLetters(name, a), bad) && !strings.HasPrefix(a, "--") {
			return false
		}
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
	// byFile holds the offsets of recursive searches of a directory that no listed path is
	// under: they run under `redact --secrets --by-file`, which masks the lines that name a
	// protected file by its name (.env, *.tfvars, ...).
	byFile []int
	ask    string
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
		// git grep searches the tracked tree: recursive without a flag.
		reader, rargs := name, words[1:]
		if name == "git" && len(words) > 1 && words[1] == "grep" {
			reader, rargs = "grep", words[2:]
		}
		gitGrep := reader != name
		recursive := gitGrep || name == "rg" || name == "grep" && slices.ContainsFunc(words[1:], func(a string) bool {
			return a == "--recursive" || a == "--dereference-recursive" || strings.ContainsAny(shortLetters("grep", a), "rR")
		})
		switch {
		case out:
			askOnce(name)
		case g.ownWrapper(words):
			cover(st) // already masked: not wrapped again, not asked
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
		case recursive && (reader == "grep" || reader == "rg") && readerOK(reader, rargs) && namesFiles(reader, rargs) && g.searchHoldsNamed(words[1:], gitGrep):
			s.byFile = append(s.byFile, int(ce.Args[0].Pos().Offset()))
			cover(st)
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

// unaccounted reports whether cmd spells a protected path (as written, from ~, $HOME or
// $CLAUDE_CONFIG_DIR;
// a glob up to its first wildcard) outside the covered byte ranges.
func (g *secretGuard) unaccounted(cmd string, covered [][2]int) bool {
	cfg := claudeConfigEnv(g.home)
	for _, p := range g.paths {
		forms := []string{p}
		if rest, ok := strings.CutPrefix(p, g.home+"/"); ok && g.home != "" {
			forms = append(forms, "~/"+rest, "$HOME/"+rest, "${HOME}/"+rest)
		}
		if rest, ok := strings.CutPrefix(p, cfg+"/"); ok && cfg != "" {
			forms = append(forms, "$CLAUDE_CONFIG_DIR/"+rest, "${CLAUDE_CONFIG_DIR}/"+rest)
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
	g := newSecretGuard(env.Home, env.XDGConfig, env.Cwd)
	g.self = env.Self
	s := g.scan(cmd)
	switch {
	case s.ask != "":
		return secretAsk("secret-file:"+s.ask, "it reads or writes a protected secret file in a way the mask cannot cover. "+
			"Plain cat, head, tail, grep, rg, sed -n 'Np' and jq . reads are masked; ls, stat, test and wc pass; "+
			`to use a value without showing it, capture it: T=$(sed -n 's/^password = "\(.*\)"$/\1/p' FILE).`)
	case len(s.wrap)+len(s.byFile) > 0:
		return decision{Rule: "secret-file:mask", Context: secretMaskNote}
	}
	return decision{}
}

// secretRewrite runs every masked reader in cmd under `self redact --secrets --`.
func secretRewrite(env *guardEnv, cmd, self string) (string, bool) {
	g := newSecretGuard(env.Home, env.XDGConfig, env.Cwd)
	g.self = self
	s := g.scan(cmd)
	if s.ask != "" || len(s.wrap)+len(s.byFile) == 0 {
		return "", false
	}
	qself, err := syntax.Quote(self, syntax.LangBash)
	if err != nil {
		return "", false
	}
	type ins struct {
		at     int
		byFile bool
	}
	var all []ins
	for _, at := range s.wrap {
		all = append(all, ins{at, false})
	}
	for _, at := range s.byFile {
		all = append(all, ins{at, true})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at > all[j].at })
	for _, w := range all {
		mode := " redact --secrets -- "
		if w.byFile {
			mode = " redact --secrets --by-file -- "
		}
		cmd = cmd[:w.at] + qself + mode + cmd[w.at:]
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
