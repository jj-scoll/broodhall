package main

// Destructive git verbs that throw away work git cannot bring back (the CLAUDE.md
// destructive-command policy): branch -D, stash (bare, drop, clear), reset --hard,
// checkout/restore of ".", clean -f. They answer with PreToolUse "ask", so the user confirms each
// one; they never block and never pass silently. Commands the shell parser rejects are read
// as loose words instead (gitDestructiveText).

import (
	"regexp"
	"strings"
)

// gitDestructive names the first destructive git invocation in cmds as rule and what it loses.
func gitDestructive(cmds []simpleCmd) (rule, what string) {
	for _, c := range cmds {
		if len(c.Words) == 0 {
			continue
		}
		sub, args := gitSub(c.Words)
		if r, w := gitDestructiveArgs(sub, args); r != "" {
			return r, w
		}
	}
	return "", ""
}

var segSplit = regexp.MustCompile(`[;&|()` + "`" + `\n]`)

// gitDestructiveText is the fallback for text the parser rejects: it cuts at separators and
// reads each `git ...` stretch as words, so it errs toward asking.
func gitDestructiveText(cmd string) (rule, what string) {
	cmd = strings.NewReplacer(`"`, " ", `'`, " ").Replace(cmd)
	for _, seg := range segSplit.Split(cmd, -1) {
		w := strings.Fields(seg)
		for i, word := range w {
			if word != "git" {
				continue
			}
			sub, args := gitSub(w[i:])
			if r, what := gitDestructiveArgs(sub, args); r != "" {
				return r, what
			}
		}
	}
	return "", ""
}

// shortFlags collects the letters of short-option clusters (-fd, -Rv) in args.
func shortFlags(args []string) string {
	var b strings.Builder
	for _, a := range args {
		if a == "--" {
			break
		}
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") {
			b.WriteString(a[1:])
		}
	}
	return b.String()
}

func hasArg(args []string, want ...string) bool {
	for _, a := range args {
		for _, w := range want {
			if a == w {
				return true
			}
		}
	}
	return false
}

func gitDestructiveArgs(sub string, args []string) (rule, what string) {
	flags := shortFlags(args)
	switch sub {
	case "branch":
		if strings.Contains(flags, "D") || (hasArg(args, "--delete") || strings.Contains(flags, "d")) && (hasArg(args, "--force") || strings.Contains(flags, "f")) {
			return "git-branch-D", "git branch -D force-deletes a branch, even one with unmerged commits"
		}
	case "stash":
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			return "git-stash", "git stash moves all uncommitted changes out of the working tree"
		}
		if args[0] == "drop" || args[0] == "clear" {
			return "git-stash", "git stash " + args[0] + " deletes saved stashes for good"
		}
	case "reset":
		if hasArg(args, "--hard") {
			return "git-reset-hard", "git reset --hard discards uncommitted changes and moves the branch"
		}
	case "checkout":
		if hasArg(args, ".") {
			return "git-checkout-dot", "git checkout . overwrites every uncommitted change in the directory"
		}
	case "restore":
		// --staged alone only unstages; --worktree (or no --staged) overwrites files.
		staged := hasArg(args, "--staged") || strings.Contains(flags, "S")
		worktree := hasArg(args, "--worktree") || strings.Contains(flags, "W")
		if hasArg(args, ".") && (!staged || worktree) {
			return "git-checkout-dot", "git restore . overwrites every uncommitted change in the directory"
		}
	case "clean":
		if (hasArg(args, "--force") || strings.Contains(flags, "f")) && !hasArg(args, "--dry-run") && !strings.Contains(flags, "n") {
			return "git-clean-f", "git clean -f deletes untracked files for good"
		}
	}
	return "", ""
}

func askDecision(rule, what string) decision {
	return decision{Ask: true, Rule: rule,
		Msg: "ASK: " + what + ". Confirm with the user before running it; prefer a reversible step (git stash push, a backup branch, git status first)."}
}
