package main

// Destructive commands outside git that delete what no undo brings back: terraform, tofu or
// terragrunt destroy (and apply -destroy), aws s3 rm --recursive and aws s3 rb --force, and
// find -delete or find -exec rm. They ask, as the destructive git verbs do (gitdestructive.go),
// and have no switch, like download-exec.

import (
	"path"
	"strings"
)

func destructiveAsk(cmd string) decision {
	cmds, ok := parseShell(cmd)
	if !ok {
		return decision{}
	}
	for _, c := range cmds {
		if rule, what := argvDestructiveAsk(c.Words); rule != "" {
			return decision{Ask: true, Rule: rule, Msg: "ASK: " + what + ". Confirm with the user before running it."}
		}
	}
	return decision{}
}

// awsArgOpts are the aws CLI's global options that take a separate value.
var awsArgOpts = map[string]bool{"--profile": true, "--region": true, "--output": true, "--endpoint-url": true,
	"--query": true, "--color": true, "--ca-bundle": true, "--cli-read-timeout": true, "--cli-connect-timeout": true,
	"--cli-binary-format": true}

// argvDestructiveAsk names the rule one argv (wrappers peeled) breaks and what it loses.
func argvDestructiveAsk(w []string) (rule, what string) {
	if len(w) == 0 {
		return "", ""
	}
	name, args := path.Base(w[0]), w[1:]
	var pos []string // the words that are not options
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case name == "aws" && awsArgOpts[a]:
			i++
		case !strings.HasPrefix(a, "-"):
			pos = append(pos, a)
		}
	}
	switch name {
	case "terraform", "tofu", "terragrunt":
		// destroy, or apply -destroy; terragrunt also as run-all destroy / run --all destroy.
		if hasArg(pos, "destroy") || hasArg(pos, "apply") && hasArg(args, "-destroy", "--destroy") {
			return "iac-destroy", name + " destroy deletes every resource the configuration manages; run " + name + " plan -destroy first and show the user what goes"
		}
	case "aws":
		if len(pos) >= 2 && pos[0] == "s3" {
			if pos[1] == "rm" && hasArg(args, "--recursive") {
				return "s3-rm", "aws s3 rm --recursive deletes every object under the prefix; unversioned buckets keep no copy (try --dryrun first)"
			}
			if pos[1] == "rb" && hasArg(args, "--force") {
				return "s3-rm", "aws s3 rb --force deletes every object in the bucket, then the bucket"
			}
		}
	case "find":
		del := hasArg(args, "-delete")
		for _, ea := range execArgs(w) {
			if e := peelWrappers(ea); len(e) > 0 && path.Base(e[0]) == "rm" {
				del = true
			}
		}
		if del {
			return "find-delete", "find -delete (or -exec rm) removes every file the expression matches, with no undo; run the same find without it first to see the list"
		}
	}
	return "", ""
}

// dynamicNames are the commands whose arguments the guard can judge on their own.
var dynamicNames = []string{"rm", "git", "chmod", "chown", "dd", "find", "terraform", "tofu", "terragrunt", "aws"}

// dynamicCommand asks when a command's name is only known at run time ("$c", $GIT,
// $(which git)) and its arguments would be destructive behind one of dynamicNames, or as a
// command of their own (the name expanding to a wrapper or to nothing). Names the line
// resolves itself were substituted by parseShell; "$EDITOR" file or ${GO:-go} build pass.
func dynamicCommand(cmd, home string) decision {
	cmds, ok := parseShell(cmd)
	if !ok {
		return decision{}
	}
	for _, c := range cmds {
		if len(c.Words) == 0 || !strings.ContainsAny(path.Base(c.Words[0]), "$`") {
			continue
		}
		cands := [][]string{c.Words[1:]}
		for _, name := range dynamicNames {
			cands = append(cands, append([]string{name}, c.Words[1:]...))
		}
		for _, w := range cands {
			if len(w) > 0 && argvDestructive(w, home) {
				return decision{Ask: true, Rule: "dynamic-command",
					Msg: "ASK: the command name is only known at run time, and with these arguments it could be " + path.Base(w[0]) + " deleting or overwriting what cannot be restored. Confirm with the user before running it."}
			}
		}
	}
	return decision{}
}

// argvDestructive reports whether any block or ask rule that reads one argv fires on w.
func argvDestructive(w []string, home string) bool {
	sc := simpleCmd{Words: w}
	sub, args := gitSub(w)
	r, _ := gitDestructiveArgs(sub, args)
	rule, _ := argvDestructiveAsk(w)
	return path.Base(w[0]) == "rm" && rmArgsTarget(w[1:], home) != "" || gitPushForce(w) != "" || r != "" || rule != "" ||
		ddRule(sc, home).Exit != 0 || chmodRule(sc, home).Exit != 0 || chownRule(sc, home).Exit != 0
}
