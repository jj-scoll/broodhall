package main

// Settings-file guard (pre-tool-use). Claude Code settings files hold the hooks and
// permission rules that every other guard depends on. A model write to one asks:
// settings.json or settings.local.json in a project's .claude directory or a Claude config
// directory (isClaudeConfigDir). Claude Code 2.1.312 stages some model edits for review, but
// not one an allow rule covers (probed: Edit with --allowedTools=Edit wrote straight to
// disk), so this asks first. config-gate (configgate.go) is the second
// line: it judges what a change does when Claude Code reloads it.
//
// Bash writes are found by fileWriteOp, the pii-rules guard's detector, so the same speed
// bump caveats apply: a script written earlier or a path built from variables gets past it.

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const settingsAskMsg = "Claude Code settings files hold the hooks and permissions that guard every session. The user confirms each change the model makes to one."

var settingsNameRE = regexp.MustCompile(`^settings(\.local)?\.json$`)

func settingsAsk(op string) decision {
	return decision{Ask: true, Rule: "settings:" + op, Msg: settingsAskMsg}
}

// claudeConfigEnv is the session's CLAUDE_CONFIG_DIR, absolute and cleaned, or "".
func claudeConfigEnv(home string) string {
	cfg := expandHome(os.Getenv("CLAUDE_CONFIG_DIR"), home)
	if !filepath.IsAbs(cfg) {
		return ""
	}
	return filepath.Clean(cfg)
}

// isClaudeConfigDir reports whether the absolute path p is a Claude config directory: the
// session's CLAUDE_CONFIG_DIR, whatever its name, or a directory right in home whose name
// starts with .claude (~/.claude, ~/.claude-work, ~/.claude-personal). Claude Code keeps no
// list of config directories, and ~/.claude-<name> is the usual name for another account's.
// The settings-file guard asks before a write to the settings in one, and the secret-file
// guard protects the credentials in each (claudeConfigDirs).
//
// Both sides are compared with symlinks resolved: on macOS /var is /private/var, so a path
// and the variable that names it can each go through a link. p is resolved only when its
// name could match, which keeps the stat calls off most words of a command.
func isClaudeConfigDir(p, home string) bool {
	if cfg := claudeConfigEnv(home); cfg != "" {
		rcfg := resolveForCompare(cfg, "/")
		if b := filepath.Base(p); p == cfg || (b == filepath.Base(cfg) || b == filepath.Base(rcfg)) && resolveForCompare(p, "/") == rcfg {
			return true
		}
	}
	if home == "" || !strings.HasPrefix(filepath.Base(p), ".claude") {
		return false
	}
	d := filepath.Dir(p)
	return d == filepath.Clean(home) || resolveForCompare(d, "/") == resolveForCompare(home, "/")
}

// expandPathVars replaces a leading $HOME or $CLAUDE_CONFIG_DIR (also in braces) in a path
// word. The hook runs with the session's environment, so these are the values the shell uses.
func expandPathVars(w, home string) string {
	cfg := claudeConfigEnv(home)
	for _, kv := range [][2]string{{"${HOME}", home}, {"$HOME", home}, {"${CLAUDE_CONFIG_DIR}", cfg}, {"$CLAUDE_CONFIG_DIR", cfg}} {
		if rest, ok := strings.CutPrefix(w, kv[0]); ok && kv[1] != "" && (rest == "" || rest[0] == '/') {
			return kv[1] + rest
		}
	}
	return w
}

// claudeConfigDirs lists the Claude config directories (isClaudeConfigDir) that can hold a
// file now: ~/.claude, CLAUDE_CONFIG_DIR, and the ~/.claude* directories that exist.
func claudeConfigDirs(home string) []string {
	var ds []string
	if home != "" {
		ds = append(ds, filepath.Join(home, ".claude"))
		ents, _ := os.ReadDir(home)
		for _, e := range ents {
			p := filepath.Join(home, e.Name())
			if strings.HasPrefix(e.Name(), ".claude") && e.Name() != ".claude" {
				if fi, err := os.Stat(p); err == nil && fi.IsDir() {
					ds = append(ds, p)
				}
			}
		}
	}
	if cfg := claudeConfigEnv(home); cfg != "" {
		ds = append(ds, cfg)
	}
	return ds
}

// isClaudeDir reports whether p holds settings Claude Code loads: a project's .claude
// directory or a Claude config directory.
func isClaudeDir(p, home string) bool {
	return filepath.Base(p) == ".claude" || isClaudeConfigDir(p, home)
}

// isSettingsPath reports whether the absolute path p is, or is a glob that can match, a
// settings file in a Claude config directory.
func isSettingsPath(p, home string) bool {
	if !isClaudeDir(filepath.Dir(p), home) {
		return false
	}
	base := filepath.Base(p)
	for _, name := range []string{"settings.json", "settings.local.json"} {
		if ok, _ := filepath.Match(base, name); ok {
			return true
		}
	}
	return false
}

var setupShells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true}

// setupBinaries are the names `setup` runs under: the binary (also by its old name) and the
// plugin launcher.
var setupBinaries = map[string]bool{"guardrails": true, "claude-tools": true, "run.sh": true}

// setupBase is the lower-cased file name of a command word, for / and \ paths, without .exe.
func setupBase(word string) string {
	w := strings.ToLower(strings.ReplaceAll(strings.Trim(word, `"'`), `\`, "/"))
	return strings.TrimSuffix(path.Base(w), ".exe")
}

// setupWriteArgv reports whether one parsed command (wrappers already peeled) runs
// `<guardrails> setup ... -write`: the binary or run.sh, directly or as the script of sh.
func setupWriteArgv(w []string) bool {
	for len(w) > 0 {
		base := setupBase(w[0])
		switch {
		case setupBinaries[base]:
			return setupWriteArgs(w[1:])
		case setupShells[base]:
			if dashC(w[1:]) != "" {
				return false // sh -c 'script': parseShell returns the script's own commands
			}
			i := 1
			for i < len(w) && strings.HasPrefix(w[i], "-") {
				i++
				if (w[i-1] == "-o" || w[i-1] == "+o") && i < len(w) {
					i++
				}
			}
			w = w[i:]
		default:
			return false
		}
	}
	return false
}

// setupWriteArgs reads the arguments after the binary: "setup", then a -write flag that Go's
// flag package would turn on (-write, --write, -write=true, -write=1, -write=T ...).
func setupWriteArgs(args []string) bool {
	if len(args) == 0 || args[0] != "setup" {
		return false
	}
	for _, a := range args[1:] {
		name := strings.TrimLeft(a, "-")
		if dashes := len(a) - len(name); dashes < 1 || dashes > 2 {
			continue
		}
		if name == "write" {
			return true
		}
		if v, ok := strings.CutPrefix(name, "write="); ok {
			if on, err := strconv.ParseBool(v); err == nil && on {
				return true
			}
		}
	}
	return false
}

// winSetupRE finds a Windows-style path to the binary (C:\x\guardrails.exe setup), which the
// shell parser reads as escapes. Only then does the text check below run on a parsed command.
var winSetupRE = regexp.MustCompile(`(?i)\\(guardrails|claude-tools|run\.sh)(\.exe)?["']?\s+setup\b`)

// embeddedSetupRE finds `<binary> setup ...` up to the end of the line or the next separator.
var embeddedSetupRE = regexp.MustCompile(`(?i)[\w./\\-]*(guardrails|claude-tools|run\.sh)(\.exe)?\s+setup\b[^\n;|&'"]*`)

// bareShell reports whether the argv is a shell given no script or -c, so it reads stdin.
func bareShell(w []string) bool {
	if len(w) == 0 || !setupShells[setupBase(w[0])] {
		return false
	}
	for _, a := range w[1:] {
		if !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "+") {
			return false
		}
		if a == "-c" {
			return false
		}
	}
	return true
}

// setupWriteText is the text-level check over one ;/&&/|/newline segment, used for Windows
// paths and for a command the parser rejects.
func setupWriteText(seg string) bool {
	trim := func(s string) string { return strings.Trim(s, `"'`+"`()") }
	toks := strings.Fields(seg)
	for i, t := range toks {
		t = trim(t)
		if t == "" || isAssignment(t) {
			continue
		}
		base := setupBase(t)
		if setupBinaries[base] {
			args := make([]string, 0, len(toks)-i-1)
			for _, a := range toks[i+1:] {
				args = append(args, trim(a))
			}
			return setupWriteArgs(args)
		}
		if _, wrap := wrapperValueOpts[base]; wrap || setupShells[base] || strings.HasPrefix(t, "-") {
			continue
		}
		return false
	}
	return false
}

// setupWriteCall reports whether the command runs `<guardrails> setup ... -write`. That
// subcommand rewrites the user's settings.json without naming the file, so it needs the same
// ask as any other model write there. The command is read with the shell parser, so quoting,
// line continuations, if/for/{ }/$( )/eval/sh -c/heredocs and wrappers (env, timeout, sudo,
// xargs ...) are seen, and quoted text that merely mentions the command is not. The binary
// must be the command word. A renamed or symlinked binary, `go run`, and a binary reached
// through a variable are not seen: the speed-bump caveat of the whole guard.
func setupWriteCall(cmd string) bool {
	cmds, ok := parseShell(cmd)
	if !ok {
		for _, seg := range segSplitRE.Split(cmd, -1) {
			if setupWriteText(seg) {
				return true
			}
		}
		return false
	}
	stdinShell := false
	for _, c := range cmds {
		if setupWriteArgv(c.Words) {
			return true
		}
		if bareShell(c.Words) {
			stdinShell = true
		}
	}
	// A shell with no script reads its commands from stdin, which a heredoc, echo or printf
	// upstream can fill. Look for the command in the text of such a pipeline.
	if stdinShell {
		for _, m := range embeddedSetupRE.FindAllString(cmd, -1) {
			if setupWriteText(m) {
				return true
			}
		}
	}
	if winSetupRE.MatchString(cmd) {
		for _, seg := range segSplitRE.Split(cmd, -1) {
			if winSetupRE.MatchString(seg) && setupWriteText(seg) {
				return true
			}
		}
	}
	return false
}

// redirOnlyRE is a redirect operator with its target in the next word (`> /dev/null`).
var redirOnlyRE = regexp.MustCompile(`^(?:\d*|&)(?:>>?|<<?|<>|>&|<&)$`)

// cdWordRE finds a cd, pushd, popd or zsh chdir word (also `\cd`, `builtin cd`, `command
// cd`), an `env -C` or `env --chdir`, or CDPATH anywhere in a segment. After one, where the
// command runs can't be known for sure.
var cdWordRE = regexp.MustCompile("(?:^|[\\s(){}!`\"'])(?:\\\\?(?:cd|pushd|popd|chdir)(?:$|[\\s(){}`\"'])|env(?:\\s.*)?\\s(?:-[A-Za-z0-9]*C|--chdir))|CDPATH")

// cdTarget returns where a segment that starts with cd or pushd goes, when that is a plain
// path: the first word that is not an option or a redirect, or ~ for a bare cd. It returns
// "" for anything else (popd, cd -, pushd +1, a cd that is not first).
func cdTarget(seg string) string {
	f := strings.Fields(strings.TrimLeft(seg, " \t({"))
	if len(f) == 0 || f[0] != "cd" && f[0] != "pushd" {
		return ""
	}
	for i := 1; i < len(f); i++ {
		switch w := f[i]; {
		case strings.ContainsAny(w, "<>"):
			if redirOnlyRE.MatchString(w) {
				i++
			}
		case w == "-", strings.HasPrefix(w, "+"): // cd -, pushd +1
			return ""
		case strings.HasPrefix(w, "-"): // -L, -P, -e, -@, --
		default:
			return strings.Trim(w, `"')}`)
		}
	}
	if f[0] == "cd" {
		return "~"
	}
	return ""
}

// settingsGuard is the Bash half of the guard.
func settingsGuard(env *guardEnv, cmd string) decision {
	if setupWriteCall(cmd) {
		return settingsAsk("guardrails setup -write")
	}
	quick := []string{".claude", "settings"}
	if cfg := claudeConfigEnv(env.Home); cfg != "" {
		quick = append(quick, "CLAUDE_CONFIG_DIR", filepath.Base(cfg), filepath.Base(resolveForCompare(cfg, "/")))
	}
	if !slices.ContainsFunc(quick, func(q string) bool { return strings.Contains(cmd, q) }) {
		return decision{}
	}
	cwd, lost := env.Cwd, false
	expand := func(tok string) string {
		tok = expandHome(expandPathVars(tok, env.Home), env.Home)
		if !filepath.IsAbs(tok) {
			tok = filepath.Join(cwd, tok)
		}
		return filepath.Clean(tok)
	}
	// is tests the path tok names. Once a cd word or CDPATH shows up (lost), a relative
	// .claude* path is also tested as ~/.claude*, so the check fails closed. .claude-plugin
	// is left out: every plugin repo has one.
	is := func(tok string, test func(p, home string) bool) bool {
		if tok == "" {
			return false
		}
		if test(expand(tok), env.Home) {
			return true
		}
		rel := filepath.Clean(tok)
		first, _, _ := strings.Cut(rel, "/")
		return lost && env.Home != "" && strings.HasPrefix(first, ".claude") && first != ".claude-plugin" &&
			test(filepath.Join(env.Home, rel), env.Home)
	}
	// From the first segment with a cd, pushd, popd or CDPATH on, the location is lost: a cd
	// may sit anywhere (`if cd`, `builtin cd`) and may fail. cdTarget adds the plain target
	// of a cd that starts a segment, so cwd follows it (a subshell's cd is kept after it
	// ends: that can only ask more). dirRef: the command names a config dir or runs in
	// one, so a bare settings.json is in one. tokSplitRE splits at braces, so ${HOME}
	// becomes $HOME first.
	cmd = strings.NewReplacer(">|", ">", "${HOME}", "$HOME", "${CLAUDE_CONFIG_DIR}", "$CLAUDE_CONFIG_DIR").Replace(cmd)
	segs := segSplitRE.Split(cmd, -1)
	cwds, losts := make([]string, len(segs)), make([]bool, len(segs))
	dirRef := false
	for i, seg := range segs {
		lost = lost || cdWordRE.MatchString(seg)
		cwds[i], losts[i] = cwd, lost
		dirRef = dirRef || isClaudeDir(cwd, env.Home)
		for _, t := range tokSplitRE.Split(seg, -1) {
			dirRef = dirRef || is(t, isClaudeDir)
		}
		if dir := cdTarget(seg); dir != "" && !strings.ContainsAny(expandHome(expandPathVars(dir, env.Home), env.Home), "$`") {
			cwd = expand(dir)
		}
	}
	dirRef = dirRef || isClaudeDir(cwd, env.Home)
	isFile := func(tok string) bool {
		return is(tok, isSettingsPath) || tok != "" && dirRef && settingsNameRE.MatchString(tok)
	}
	isDir := func(tok string) bool { return is(tok, isClaudeDir) }
	// Per segment, so a pass below can't hide a write later in the command. rmdir removes
	// only empty directories, and find writes only with -delete or a writing -exec
	// (guard-replay: `rmdir .claude/worktrees .claude`, `find ~/.claude -exec ls -l {} \;`).
	for i, seg := range segs {
		cwd, lost = cwds[i], losts[i]
		switch op := fileWriteOp(seg, isFile, isDir); {
		case op == "", op == "rmdir", op == "find" && !findWrites(seg):
		default:
			return settingsAsk(op)
		}
	}
	return decision{}
}

// findWrites reports whether a find segment deletes or runs a writing command on what it finds.
func findWrites(seg string) bool {
	toks := strings.Fields(seg)
	for i, t := range toks {
		switch t {
		case "-delete":
			return true
		case "-exec", "-execdir", "-ok", "-okdir":
			if i+1 < len(toks) {
				name := filepath.Base(strings.Trim(toks[i+1], `"'`))
				if editorOps[name] || destOps[name] || name == "sed" || interpreterRE.MatchString(name) || shells[name] {
					return true
				}
			}
		}
	}
	return false
}

// settingsFileToolDecision is the Edit/Write half: tool_input of one non-Bash tool call.
func settingsFileToolDecision(tool string, input map[string]any, cwd, home string) (decision, bool) {
	switch tool {
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
	default:
		return decision{}, false
	}
	for _, k := range []string{"file_path", "notebook_path"} {
		p, _ := input[k].(string)
		if p == "" {
			continue
		}
		p = expandHome(p, home)
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		if isSettingsPath(filepath.Clean(p), home) || isSettingsPath(resolveForCompare(p, cwd), home) {
			return settingsAsk(tool), true
		}
	}
	return decision{}, false
}
