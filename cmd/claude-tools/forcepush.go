package main

// Force-push rule: reads each `git push`'s own flags instead of the legacy substring test,
// so --force-with-lease and --force-if-includes (which refuse to overwrite work you have not
// seen) pass, while a bare --force or -f anywhere in the push blocks, including the
// `git push origin --force` and `git -C dir push -f` forms the substring missed.

import (
	"path"
	"strings"
)

// gitArgOpts are git's global options that take a separate value (-C dir, -c k=v, ...).
var gitArgOpts = map[string]bool{"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true, "--config-env": true}

// pushArgOpts are git push's long options that take a separate value.
var pushArgOpts = map[string]bool{"--repo": true, "--receive-pack": true, "--exec": true, "--push-option": true}

// forcePushFlag reports the force flag ("--force" or "-f") of the first force push in cmd.
func forcePushFlag(cmd string) string {
	return shellFind(cmd, gitPushForce, func(cmd string) string {
		// Cannot parse: as strict as the legacy substrings.
		if strings.Contains(cmd, "git push --force") {
			return "--force"
		}
		if strings.Contains(cmd, "git push -f ") {
			return "-f"
		}
		return ""
	})
}

// gitSub returns git's subcommand and the arguments after it, skipping global options.
func gitSub(w []string) (sub string, args []string) {
	if path.Base(w[0]) != "git" {
		return "", nil
	}
	args = w[1:]
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		a := args[0]
		args = args[1:]
		if gitArgOpts[a] && len(args) > 0 {
			args = args[1:]
		}
	}
	if len(args) == 0 {
		return "", nil
	}
	return args[0], args[1:]
}

func gitPushForce(w []string) string {
	sub, rest := gitSub(w)
	if sub != "push" {
		return ""
	}
	args := append([]string{sub}, rest...)
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return ""
		case a == "--force":
			return "--force"
		case pushArgOpts[a]:
			i++
		case strings.HasPrefix(a, "--") || a == "-":
		case strings.HasPrefix(a, "-"):
			for j, c := range a[1:] {
				if c == 'f' {
					return "-f"
				}
				if c == 'o' { // -o takes the rest of the cluster, or the next word
					if j == len(a)-2 {
						i++
					}
					break
				}
			}
		}
	}
	return ""
}
