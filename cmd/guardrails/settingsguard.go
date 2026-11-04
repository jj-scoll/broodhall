package main

// Settings-file guard (pre-tool-use). Claude Code settings files hold the hooks and
// permission rules that every other guard depends on, and ~/.claude/settings.json is shared
// with ~/.claude-work. A model write to one asks: settings.json or settings.local.json in a
// .claude or .claude-work directory (user or project). Claude Code 2.1.312 stages some model
// edits for review, but not one an allow rule covers (probed: Edit with --allowedTools=Edit
// wrote straight to disk), so this asks first. config-gate (configgate.go) is the second
// line: it judges what a change does when Claude Code reloads it.
//
// Bash writes are found by fileWriteOp, the pii-rules guard's detector, so the same speed
// bump caveats apply: a script written earlier or a path built from variables gets past it.

import (
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const settingsAskMsg = "Claude Code settings files hold the hooks and permissions that guard every session. The user confirms each change the model makes to one."

var settingsNameRE = regexp.MustCompile(`^settings(\.local)?\.json$`)

func settingsAsk(op string) decision {
	return decision{Ask: true, Rule: "settings:" + op, Msg: settingsAskMsg}
}

func isClaudeDir(p string) bool {
	b := filepath.Base(p)
	return b == ".claude" || b == ".claude-work"
}

// isSettingsPath reports whether the absolute path p is, or is a glob that can match, a
// settings file in a Claude config directory.
func isSettingsPath(p string) bool {
	if !isClaudeDir(filepath.Dir(p)) {
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

// settingsGuard is the Bash half of the guard.
func settingsGuard(env *guardEnv, cmd string) decision {
	if setupWriteCall(cmd) {
		return settingsAsk("guardrails setup -write")
	}
	if !strings.Contains(cmd, ".claude") && !strings.Contains(cmd, "settings") {
		return decision{}
	}
	expand := func(tok string) string {
		for _, v := range []string{"$HOME", "${HOME}"} {
			tok = strings.ReplaceAll(tok, v, env.Home)
		}
		tok = expandHome(tok, env.Home)
		if !filepath.IsAbs(tok) {
			tok = filepath.Join(env.Cwd, tok)
		}
		return filepath.Clean(tok)
	}
	// dirRef: the command (or the cwd) is in a config dir, so a bare settings.json is one.
	dirRef := isClaudeDir(env.Cwd)
	for _, t := range tokSplitRE.Split(cmd, -1) {
		dirRef = dirRef || t != "" && isClaudeDir(expand(t))
	}
	isFile := func(tok string) bool {
		return tok != "" && (isSettingsPath(expand(tok)) || dirRef && settingsNameRE.MatchString(tok))
	}
	isDir := func(tok string) bool { return tok != "" && isClaudeDir(expand(tok)) }
	// Per segment, so a pass below can't hide a write later in the command. rmdir removes
	// only empty directories, and find writes only with -delete or a writing -exec
	// (guard-replay: `rmdir .claude/worktrees .claude`, `find ~/.claude -exec ls -l {} \;`).
	for _, seg := range segSplitRE.Split(strings.ReplaceAll(cmd, ">|", ">"), -1) {
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
		if isSettingsPath(filepath.Clean(p)) || isSettingsPath(resolveForCompare(p, cwd)) {
			return settingsAsk(tool), true
		}
	}
	return decision{}, false
}
