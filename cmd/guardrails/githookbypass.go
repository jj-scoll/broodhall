package main

// Git commands that skip hooks or signing: --no-verify (and -n where git means that by it),
// --no-gpg-sign, -c core.hooksPath=..., git config core.hooksPath or commit.gpgsign=false,
// and the hook runners' off switches in the environment (HUSKY=0, SKIP=..., LEFTHOOK=0).
// A failing hook or a signing error is a message from the repository's owner; skipping it
// is a decision for the user, so these ask, like the destructive verbs in gitdestructive.go,
// and they sit in the same `git` family. Commands the shell parser rejects are read as loose
// words, as there.
//
// -n is no-verify only for commit and am (git help commit, git help am). For push it is
// --dry-run, for merge, rebase and pull --no-stat, for revert and cherry-pick --no-commit.

import (
	"path"
	"strings"
)

// hookSubs are the git subcommands that run hooks the flags and variables below can skip.
var hookSubs = map[string]bool{"commit": true, "push": true, "merge": true, "rebase": true, "am": true, "cherry-pick": true, "revert": true, "pull": true}

// shortNoVerify are the subcommands whose -n means --no-verify.
var shortNoVerify = map[string]bool{"commit": true, "am": true}

// noVerifyLong are the subcommands that take --no-verify.
var noVerifyLong = map[string]bool{"commit": true, "am": true, "push": true, "merge": true, "rebase": true, "pull": true}

// noSignFlag is each subcommand's option that turns signing off.
var noSignFlag = map[string]string{"commit": "--no-gpg-sign", "merge": "--no-gpg-sign", "rebase": "--no-gpg-sign",
	"cherry-pick": "--no-gpg-sign", "revert": "--no-gpg-sign", "am": "--no-gpg-sign", "pull": "--no-gpg-sign", "tag": "--no-sign"}

// envSkips are the variables that switch a hook runner off, with the values that do it
// (empty: any non-empty value).
var envSkips = map[string]string{"HUSKY": "0", "HUSKY_SKIP_HOOKS": "1", "LEFTHOOK": "0", "SKIP": ""}

// shortValued are the short options of commit and am that take a value, in a cluster or as
// the next word; attachedOnly ones (-S, -u) take only what follows them in the same word.
const shortValued, shortAttached = "mFCctpd", "Su"

// gitHookBypass names the first hook- or signing-skipping git invocation in cmds.
func gitHookBypass(cmds []simpleCmd, cwd string) (rule, what string) {
	var exported []string // export NAME=value anywhere: assumed to reach every git command
	for _, c := range cmds {
		if len(c.Words) > 0 && c.Words[0] == "export" {
			exported = append(exported, c.Env...)
		}
	}
	for _, c := range cmds {
		if len(c.Words) == 0 {
			continue
		}
		if w := gitHookBypassWords(c.Words, append(append([]string{}, c.Env...), exported...), cwd); w != "" {
			return "git-hook-bypass", w
		}
	}
	return "", ""
}

// gitHookBypassText is the fallback for text the parser rejects.
func gitHookBypassText(cmd, cwd string) (rule, what string) {
	cmd = strings.NewReplacer(`"`, " ", `'`, " ").Replace(cmd)
	for _, seg := range segSplit.Split(cmd, -1) {
		w, env := peelWrappersEnv(strings.Fields(seg))
		for i, word := range w {
			if word != "git" {
				continue
			}
			if what := gitHookBypassWords(w[i:], env, cwd); what != "" {
				return "git-hook-bypass", what
			}
		}
	}
	return "", ""
}

func gitHookBypassWords(w, env []string, cwd string) string {
	sub, args := gitSub(w)
	if sub == "" {
		return ""
	}
	signs := noSignFlag[sub] != ""
	for _, kv := range append(gitGlobalConfigs(w), envGitConfigs(env)...) {
		if what := configBypass(kv, cwd, signs); what != "" {
			return "git -c " + kv + " " + what
		}
	}
	switch sub {
	case "config":
		if key, val, set := gitConfigSet(args); set {
			if what := configBypass(key+"="+val, cwd, true); what != "" {
				return "git config " + key + " " + val + " " + what
			}
		}
		return ""
	}
	if hookSubs[sub] {
		for _, kv := range env {
			name, val, _ := strings.Cut(kv, "=")
			if want, ok := envSkips[name]; ok && (want == "" && val != "" || want != "" && val == want) {
				return kv + " switches the repository's hooks off for git " + sub
			}
		}
	}
	if noVerifyArgs(sub, args) {
		return "git " + sub + " --no-verify skips the repository's hooks"
	}
	if flag := noSignFlag[sub]; flag != "" && hasArgBeforeDashes(args, flag) {
		return "git " + sub + " " + flag + " turns commit signing off"
	}
	return ""
}

// noVerifyArgs reports whether args of sub end with hooks skipped: the last of --no-verify
// (and -n for commit and am) and --verify wins.
func noVerifyArgs(sub string, args []string) bool {
	if !noVerifyLong[sub] {
		return false
	}
	skipped := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return skipped
		case a == "--no-verify":
			skipped = true
		case a == "--verify":
			skipped = false
		case strings.HasPrefix(a, "--") || !strings.HasPrefix(a, "-") || a == "-":
		case shortNoVerify[sub]:
			// A cluster such as -anm: letters up to the first one that takes a value.
		cluster:
			for j := 1; j < len(a); j++ {
				switch c := a[j]; {
				case c == 'n':
					skipped = true
				case strings.IndexByte(shortAttached, c) >= 0:
					break cluster
				case strings.IndexByte(shortValued, c) >= 0:
					if j == len(a)-1 {
						i++
					}
					break cluster
				}
			}
		}
	}
	return skipped
}

func hasArgBeforeDashes(args []string, want string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == want {
			return true
		}
	}
	return false
}

// gitGlobalConfigs lists the key=value settings given as git's own -c (and --config-env,
// whose value is an environment variable name) before the subcommand.
func gitGlobalConfigs(w []string) []string {
	var out []string
	args := w[1:]
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		a := args[0]
		args = args[1:]
		switch {
		case a == "-c" && len(args) > 0:
			out = append(out, args[0])
			args = args[1:]
		case strings.HasPrefix(a, "-c") && !strings.HasPrefix(a, "--"):
			out = append(out, a[2:]) // -ccore.hooksPath=x
		case a == "--config-env" && len(args) > 0:
			out = append(out, configEnvKV(args[0]))
			args = args[1:]
		case strings.HasPrefix(a, "--config-env="):
			out = append(out, configEnvKV(strings.TrimPrefix(a, "--config-env=")))
		case gitArgOpts[a] && len(args) > 0:
			args = args[1:]
		}
	}
	return out
}

// configEnvKV turns --config-env key=ENVVAR into key=$ENVVAR: the value is unknown.
func configEnvKV(kv string) string {
	key, name, ok := strings.Cut(kv, "=")
	if !ok {
		return kv
	}
	return key + "=$" + name
}

// envGitConfigs lists the settings git reads from the environment: GIT_CONFIG_PARAMETERS
// (see gitConfigParams) and GIT_CONFIG_COUNT with GIT_CONFIG_KEY_n / GIT_CONFIG_VALUE_n.
func envGitConfigs(env []string) []string {
	var out []string
	keys, vals := map[string]string{}, map[string]string{}
	for _, kv := range env {
		name, val, _ := strings.Cut(kv, "=")
		switch {
		case name == "GIT_CONFIG_PARAMETERS":
			out = append(out, gitConfigParams(val)...)
		case strings.HasPrefix(name, "GIT_CONFIG_KEY_"):
			keys[strings.TrimPrefix(name, "GIT_CONFIG_KEY_")] = val
		case strings.HasPrefix(name, "GIT_CONFIG_VALUE_"):
			vals[strings.TrimPrefix(name, "GIT_CONFIG_VALUE_")] = val
		}
	}
	for n, k := range keys {
		out = append(out, k+"="+vals[n])
	}
	return out
}

// gitSpace is what git's isspace matches (git-compat-util.h).
const gitSpace = " \t\n\r"

// gitConfigParams reads GIT_CONFIG_PARAMETERS the way git's parse_config_env_list
// (config.c) does: entries split by spaces, each 'key=value' (the old form), 'key'='value'
// (the form git 2.31 and later writes) or 'key'= (true). Each comes back as key=value, or as
// key alone when it has no value. It stops at the first entry git rejects: git then fails
// with "bogus format", so the command runs nothing.
func gitConfigParams(s string) []string {
	var out []string
	for s != "" {
		key, rest, ok := sqDequoteStep(s)
		if !ok {
			return out
		}
		switch {
		case rest == "" || strings.IndexByte(gitSpace, rest[0]) >= 0:
			out = append(out, key) // old form: git splits it at the first =
		case rest[0] == '=' && (len(rest) == 1 || strings.IndexByte(gitSpace, rest[1]) >= 0):
			out = append(out, key) // 'key'=
			rest = rest[1:]
		case rest[0] == '=':
			val, r, ok := sqDequoteStep(rest[1:])
			if !ok || r != "" && strings.IndexByte(gitSpace, r[0]) < 0 {
				return out
			}
			out = append(out, key+"="+val)
			rest = r
		default:
			return out
		}
		s = strings.TrimLeft(rest, gitSpace)
	}
	return out
}

// sqDequoteStep is git's sq_dequote_step (quote.c): one single-quoted word from the start of
// s, where a backslashed quote or ! between two quoted parts stands for itself, and the text
// after it. ok is false when s does not start with a quote or the quote is not closed.
func sqDequoteStep(s string) (word, rest string, ok bool) {
	if s == "" || s[0] != '\'' {
		return "", "", false
	}
	var b strings.Builder
	for i := 1; ; {
		j := strings.IndexByte(s[i:], '\'')
		if j < 0 {
			return "", "", false
		}
		b.WriteString(s[i : i+j])
		i += j + 1
		if i+2 < len(s) && s[i] == '\\' && (s[i+1] == '\'' || s[i+1] == '!') && s[i+2] == '\'' {
			b.WriteByte(s[i+1])
			i += 3
			continue
		}
		return b.String(), s[i:], true
	}
}

// hooksPathLeavesRepo reports whether a core.hooksPath value switches the repository's
// hooks off or moves them out of reach: empty, anything under /dev, an unknown value (from
// the environment or a variable), ~, an absolute path outside the working directory, or a
// relative path that climbs out of it. A directory inside the repo (.githooks, .husky) is the
// usual husky and githooks setup and passes.
func hooksPathLeavesRepo(val, cwd string) bool {
	switch {
	case val == "" || strings.HasPrefix(val, "/dev/") || val == "/dev" || strings.HasPrefix(val, "$") || strings.HasPrefix(val, "~"):
		return true
	case strings.HasPrefix(val, "/"):
		return cwd == "" || !(path.Clean(val) == path.Clean(cwd) || strings.HasPrefix(path.Clean(val), path.Clean(cwd)+"/"))
	}
	c := path.Clean(val)
	return c == ".." || strings.HasPrefix(c, "../")
}

// configBypass says why a key=value git setting skips hooks or signing; "" if it does not.
// signs says the command makes a commit or tag, which is where a signing setting matters.
// A setting with no value (-c commit.gpgsign) means true.
func configBypass(kv, cwd string, signs bool) string {
	key, val, hasVal := strings.Cut(kv, "=")
	switch strings.ToLower(key) {
	case "core.hookspath":
		if hooksPathLeavesRepo(val, cwd) {
			return "disables the repository's hooks or points git at hooks outside the repository"
		}
	case "commit.gpgsign", "tag.gpgsign", "push.gpgsign":
		switch strings.ToLower(val) {
		case "false", "no", "off", "0", "":
			if hasVal && signs {
				return "turns signing off"
			}
		}
	}
	return ""
}

// gitConfigValued are git config's options that take a separate value.
var gitConfigValued = map[string]bool{"-f": true, "--file": true, "--blob": true, "--type": true, "--default": true, "--comment": true}

// gitConfigSet reports whether `git config args` writes a value, and which one. The reads
// and removals (--get, --list, --unset, ...) are not sets, so they pass.
func gitConfigSet(args []string) (key, val string, set bool) {
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case gitConfigValued[a]:
			i++
		case strings.HasPrefix(a, "--get"), strings.HasPrefix(a, "--unset"), strings.HasPrefix(a, "--remove"), strings.HasPrefix(a, "--rename"),
			a == "--list", a == "-l", a == "--edit", a == "-e":
			return "", "", false
		case strings.HasPrefix(a, "-"):
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) > 0 {
		switch pos[0] {
		case "set":
			pos = pos[1:]
		case "get", "unset", "list", "edit", "rename-section", "remove-section":
			return "", "", false
		}
	}
	if len(pos) < 2 {
		return "", "", false
	}
	return pos[0], pos[1], true
}
