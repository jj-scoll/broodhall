package main

// PreToolUse guard for the PII rules file (see piirules.go). Masking protects the model from
// client data, so the model must not be able to loosen the rules that do the masking: the user
// owns ~/.config/claude-tools/pii-columns.json.
//
//   - Edit/Write/MultiEdit/NotebookEdit: the target path is resolved (~, relative, .., and
//     symlinks of the deepest existing ancestor) and compared with the rules file.
//   - Bash: heuristic. A segment (split on ; && || | newline) that names the rules file and
//     writes it is blocked: redirects into it, sed -i, tee, mv, rm, chmod, truncate, cp/
//     install/ln/rsync with it as destination, editors, and interpreters (python, perl...)
//     that name it next to a write marker. Reading (cat, rg, jq, cp FROM it) passes.
//   - Bash assignments of XDG_CONFIG_HOME or HOME are blocked when the command also runs
//     claude-tools: either would point the loader at a file the model wrote.
//
// None of this is a security boundary: the model runs as the user and can write the file by
// ways no regex sees (a script written earlier, indirection through variables). It makes
// the obvious routes fail loudly so they happen only by deliberate circumvention.

import (
	"path/filepath"
	"regexp"
	"strings"
)

const rulesDenyMsg = "PII rules (~/.config/claude-tools/pii-columns.json) are the user's to edit, not the model's."

func rulesDeny(rule, extra string) decision {
	m := rulesDenyMsg
	if extra != "" {
		m += " " + extra
	}
	return decision{Exit: 2, Rule: rule, Msg: "BLOCKED: " + m}
}

// rulesFiles is every path the loader may read: the HOME one and the XDG one.
func rulesFiles(home, xdg string) []string {
	files := []string{}
	for _, x := range []string{"", xdg} {
		if p := piiRulesPathFor(x, home); p != "" {
			dup := false
			for _, q := range files {
				dup = dup || q == p
			}
			if !dup {
				files = append(files, p)
			}
		}
	}
	return files
}

// resolveForCompare makes p absolute and follows symlinks as far as the path exists.
func resolveForCompare(p, cwd string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	p = filepath.Clean(p)
	rest := ""
	for q := p; ; q = filepath.Dir(q) {
		if r, err := filepath.EvalSymlinks(q); err == nil {
			return filepath.Join(r, rest)
		}
		rest = filepath.Join(filepath.Base(q), rest)
		if filepath.Dir(q) == q {
			return p
		}
	}
}

func expandHome(p, home string) string {
	switch {
	case p == "~":
		return home
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(home, p[2:])
	}
	return p
}

// isRulesFilePath reports whether a tool's file_path names the rules file.
func isRulesFilePath(p, cwd, home, xdg string) bool {
	if p == "" {
		return false
	}
	got := resolveForCompare(expandHome(p, home), cwd)
	for _, f := range rulesFiles(home, xdg) {
		if got == resolveForCompare(f, cwd) {
			return true
		}
	}
	return false
}

var (
	envAssignRE = regexp.MustCompile(`(^|[\s;&|('"])(XDG_CONFIG_HOME|HOME)=`)
	varAssignRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	cmdWrappers = map[string]bool{"env": true, "export": true, "sudo": true, "time": true, "nohup": true, "exec": true,
		"command": true, "nice": true, "xargs": true, "builtin": true}
	shells        = map[string]bool{"bash": true, "sh": true, "zsh": true, "dash": true}
	segSplitRE    = regexp.MustCompile(`\n|;|&&|\|\||\|`)
	tokSplitRE    = regexp.MustCompile(`[\s"'<>=(),{};|&]+`)
	redirectRE    = regexp.MustCompile(`(?:\d*|&)>>?\|?\s*(?:"([^"]*)"|'([^']*)'|([^\s;|&<>]+))`)
	interpreterRE = regexp.MustCompile(`^(python[0-9.]*|perl|ruby|node|nodejs|php|lua|gawk|awk|osascript)$`)
	writeMarkerRE = regexp.MustCompile(`['"][wax]\+?b?['"]|write|truncate|rename|replace|unlink|remove|rmtree|shutil|copy|move|dump|-[a-zA-Z]*i\b|inplace`)
	editorOps     = map[string]bool{"tee": true, "rm": true, "rmdir": true, "mv": true, "chmod": true, "chown": true, "chattr": true,
		"truncate": true, "shred": true, "unlink": true, "sponge": true, "ed": true, "ex": true, "vi": true, "vim": true,
		"nvim": true, "nano": true, "emacs": true, "patch": true, "dd": true, "setfacl": true}
	destOps = map[string]bool{"cp": true, "install": true, "ln": true, "rsync": true}
)

// runsClaudeTools reports whether any segment of cmd executes the claude-tools binary (any
// path). The command word is found after env/export/sudo-style wrappers, flags and VAR=
// assignments; a shell segment (bash -c ...) is scanned in full since its string is a command.
func runsClaudeTools(cmd string) bool {
	isTool := func(tok string) bool {
		tok = strings.Trim(tok, "\"'`$()")
		return filepath.Base(tok) == "claude-tools"
	}
	for _, seg := range segSplitRE.Split(cmd, -1) {
		fields := strings.Fields(seg)
		for i, tok := range fields {
			bare := strings.Trim(tok, "\"'`$()")
			if bare == "" || varAssignRE.MatchString(bare) || strings.HasPrefix(bare, "-") || cmdWrappers[bare] {
				continue
			}
			if isTool(tok) {
				return true
			}
			if shells[filepath.Base(bare)] {
				for _, rest := range fields[i+1:] {
					if isTool(rest) {
						return true
					}
				}
			}
			break
		}
	}
	return false
}

// piiRulesGuard is the Bash half of the guard.
func piiRulesGuard(env *guardEnv, cmd string) decision {
	if envAssignRE.MatchString(cmd) && runsClaudeTools(cmd) {
		return rulesDeny("pii-rules:env", "Setting XDG_CONFIG_HOME or HOME would point claude-tools at a different rules file.")
	}
	files := rulesFiles(env.Home, env.XDGConfig)
	if len(files) == 0 || !strings.Contains(cmd, "pii-columns") && !strings.Contains(cmd, "claude-tools") {
		return decision{}
	}
	var dirs []string
	for _, f := range files {
		dirs = append(dirs, filepath.Dir(f))
	}
	expand := func(tok string) string {
		for _, v := range []string{"$HOME", "${HOME}"} {
			tok = strings.ReplaceAll(tok, v, env.Home)
		}
		if env.XDGConfig != "" {
			tok = strings.ReplaceAll(strings.ReplaceAll(tok, "${XDG_CONFIG_HOME}", env.XDGConfig), "$XDG_CONFIG_HOME", env.XDGConfig)
		}
		return filepath.Clean(expandHome(tok, env.Home))
	}
	// dirRef: the command (or the cwd) is in the config dir, so a bare pii-columns.json is ours.
	dirRef := false
	for _, d := range dirs {
		dirRef = dirRef || env.Cwd == d
	}
	for _, t := range tokSplitRE.Split(cmd, -1) {
		for _, d := range dirs {
			if t != "" && (expand(t) == d || strings.HasPrefix(expand(t), d+"/")) {
				dirRef = true
			}
		}
	}
	isFile := func(tok string) bool {
		if tok == "" {
			return false
		}
		e := expand(tok)
		for _, f := range files {
			if e == f {
				return true
			}
		}
		for _, d := range dirs {
			if strings.HasPrefix(e, d+"/") && strings.ContainsAny(e, "*?[") {
				return true // glob that can match the file
			}
		}
		return dirRef && (tok == piiRulesFile || tok == "./"+piiRulesFile)
	}
	isDir := func(tok string) bool {
		for _, d := range dirs {
			if tok != "" && expand(tok) == d {
				return true
			}
		}
		return false
	}
	if op := fileWriteOp(cmd, isFile, isDir); op != "" {
		return rulesDeny("pii-rules:"+op, "")
	}
	return decision{}
}

// fileWriteOp names how cmd writes a file isFile matches, or a directory isDir matches
// (redirect, sed, tee, cp, find, interpreter...); "" when no segment does.
func fileWriteOp(cmd string, isFile, isDir func(string) bool) string {
	for _, seg := range segSplitRE.Split(strings.ReplaceAll(cmd, ">|", ">"), -1) {
		for _, m := range redirectRE.FindAllStringSubmatch(seg, -1) {
			if isFile(m[1] + m[2] + m[3]) {
				return "redirect"
			}
		}
		var toks []string
		for _, t := range tokSplitRE.Split(seg, -1) {
			if t != "" {
				toks = append(toks, t)
			}
		}
		var fileTok, dirTok bool
		for _, t := range toks {
			fileTok = fileTok || isFile(t)
			dirTok = dirTok || isDir(t)
		}
		if !fileTok && !dirTok {
			continue
		}
		for i, t := range toks {
			name := filepath.Base(t)
			switch {
			case name == "sed" && fileTok:
				for _, a := range toks[i+1:] {
					if a == "--in-place" || strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "i") {
						return "sed"
					}
				}
			case editorOps[name]:
				return name
			case destOps[name]:
				if dest := destination(toks[i+1:]); isFile(dest) || isDir(dest) {
					return name
				}
			case name == "find" && dirTok:
				for _, a := range toks[i+1:] {
					if a == "-delete" || a == "-exec" {
						return "find"
					}
				}
			case interpreterRE.MatchString(name) && fileTok && writeMarkerRE.MatchString(seg):
				return "interpreter"
			}
		}
	}
	return ""
}

// destination is the target of cp/install/ln/rsync: the -t argument, else the last non-flag word.
func destination(args []string) string {
	last := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case (a == "-t" || a == "--target-directory") && i+1 < len(args):
			return args[i+1]
		case strings.HasPrefix(a, "-"):
		default:
			last = a
		}
	}
	return last
}

// rulesFileToolDeny is the Edit/Write half: tool_input of one non-Bash tool call.
func rulesFileToolDeny(tool string, input map[string]any, cwd, home, xdg string) (decision, bool) {
	switch tool {
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
	default:
		return decision{}, false
	}
	for _, k := range []string{"file_path", "notebook_path"} {
		if p, _ := input[k].(string); isRulesFilePath(p, cwd, home, xdg) {
			return rulesDeny("pii-rules:"+tool, ""), true
		}
	}
	return decision{}, false
}
