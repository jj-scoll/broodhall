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
	"path/filepath"
	"regexp"
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

// setupWrappers are command words that run the next word: skipped to find the real command.
var setupWrappers = map[string]bool{"env": true, "exec": true, "command": true, "sudo": true, "time": true, "nohup": true, "nice": true}

var setupShells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true}

// setupBinaries are the names `setup` runs under: the binary (also by its old name) and the
// plugin launcher.
var setupBinaries = map[string]bool{"guardrails": true, "claude-tools": true, "run.sh": true}

// setupWriteCall reports whether the command segment runs `<guardrails> setup ... -write`.
// That subcommand rewrites the user's settings.json without naming the file, so it needs the
// same ask as any other model write there. The binary must be the command word (after env
// assignments and wrappers such as sh, env, exec), so a commit message, an echo or a grep that
// only mentions it does not ask. A binary reached through a variable is not seen: the same
// speed-bump caveat as the rest of this guard.
func setupWriteCall(seg string) bool {
	toks := strings.Fields(seg)
	trim := func(s string) string { return strings.Trim(s, `"'`+"`()") }
	for i := 0; i < len(toks); i++ {
		t := trim(toks[i])
		base := strings.TrimSuffix(filepath.Base(strings.ReplaceAll(t, `\`, "/")), ".exe")
		switch {
		case t == "" || strings.Contains(t, "=") && !strings.ContainsAny(t, "/\\") && !strings.HasPrefix(t, "-"):
			continue // VAR=value
		case setupWrappers[base] && !setupBinaries[base]:
			continue
		case setupShells[base]:
			for j := i + 1; j < len(toks); j++ {
				if trim(toks[j]) == "-c" {
					return setupWriteCall(strings.Join(toks[j+1:], " "))
				}
			}
			continue // sh script.sh setup ...: the script is the next word
		case setupBinaries[base]:
			if i+1 >= len(toks) || trim(toks[i+1]) != "setup" {
				return false
			}
			for _, a := range toks[i+2:] {
				a = trim(a)
				if a == "-write" || a == "--write" {
					return true
				}
				if v, ok := strings.CutPrefix(strings.TrimLeft(a, "-"), "write="); ok && strings.HasPrefix(a, "-") {
					switch strings.ToLower(v) {
					case "false", "0", "f", "":
					default:
						return true
					}
				}
			}
			return false
		default:
			return false
		}
	}
	return false
}

// settingsGuard is the Bash half of the guard.
func settingsGuard(env *guardEnv, cmd string) decision {
	for _, seg := range segSplitRE.Split(cmd, -1) {
		if setupWriteCall(seg) {
			return settingsAsk("guardrails setup -write")
		}
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
