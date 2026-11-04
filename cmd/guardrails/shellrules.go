package main

// shellRules ports the legacy substring blocklist entries that rm and force-push did not
// already replace (> /etc/, dd if=, the fork bomb, chmod -R 777 /, chown -R, privileged
// containers) to parsed commands: the same intent, but only a real invocation counts, not
// text quoted in an echo, a commit message, a grep pattern or a heredoc body. A command
// mvdan cannot parse gets the legacy substring decision.

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

func dangerous(p string) decision {
	return decision{Exit: 2, Rule: "dangerous:" + p,
		Msg: fmt.Sprintf("BLOCKED: '%s' is a high-risk destructive operation. Confirm with the user before attempting this.", p)}
}

func shellRules(env *guardEnv, cmd string) decision {
	cmds, ok := parseShell(cmd)
	if !ok {
		return legacyRules(cmd, supersededLegacy, true)
	}
	// One pass per rule, in the legacy order, so the reported rule matches the old one.
	rules := []func(simpleCmd, string) decision{etcWriteRule, ddRule, forkBombRule, chmodRule, chownRule, privilegedRule}
	for _, rule := range rules {
		for _, c := range cmds {
			if d := rule(c, env.Home); d.Exit != 0 {
				return d
			}
		}
	}
	return decision{}
}

func argv0(c simpleCmd) string {
	if len(c.Words) == 0 {
		return ""
	}
	return path.Base(c.Words[0])
}

// etcWriteRule: output redirected into /etc/.
func etcWriteRule(c simpleCmd, _ string) decision {
	for _, r := range c.Redirs {
		switch r.Op {
		case ">", ">>", ">|", "&>", "&>>":
			if strings.HasPrefix(r.Target, "/etc/") {
				return dangerous("> /etc/")
			}
		}
	}
	return decision{}
}

// ddRule: dd reading an if= input.
func ddRule(c simpleCmd, _ string) decision {
	if argv0(c) == "dd" {
		for _, a := range c.Words[1:] {
			if strings.HasPrefix(a, "if=") {
				return dangerous("dd if=")
			}
		}
	}
	return decision{}
}

func forkBombRule(c simpleCmd, _ string) decision {
	if c.ForkBomb {
		return dangerous(":(){ :|:& };:")
	}
	return decision{}
}

// recursiveFlag reports -R, --recursive or a short cluster holding R (-Rv, -hR).
func recursiveFlag(a string) bool {
	return a == "--recursive" || strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "R")
}

var openMode = regexp.MustCompile(`^(0?777|(a|ugo)\+rwx)$`)

// chmodRule: recursive world-writable mode on a protected target (/, ~, $HOME, /home, /nix,
// /storage), the same targets as the rm rule. The old substring also hit /var/www/x.
func chmodRule(c simpleCmd, home string) decision {
	if argv0(c) != "chmod" {
		return decision{}
	}
	var recursive, open bool
	var targets []string
	for _, a := range c.Words[1:] {
		switch {
		case recursiveFlag(a):
			recursive = true
		case strings.HasPrefix(a, "-"):
		case openMode.MatchString(a):
			open = true
		default:
			targets = append(targets, a)
		}
	}
	if recursive && open {
		for _, t := range targets {
			if rootName(t, home) != "" {
				return dangerous("chmod -R 777 /")
			}
		}
	}
	return decision{}
}

// chownRule: recursive chown of a protected target, the same targets as the rm and chmod
// rules. The old substring blocked every chown -R, scratch dirs and containers' own files too.
func chownRule(c simpleCmd, home string) decision {
	if argv0(c) != "chown" {
		return decision{}
	}
	var recursive bool
	var targets []string
	for _, a := range c.Words[1:] {
		switch {
		case recursiveFlag(a):
			recursive = true
		case strings.HasPrefix(a, "-"):
		default:
			targets = append(targets, a)
		}
	}
	if recursive {
		for _, t := range targets {
			if rootName(t, home) != "" {
				return dangerous("chown -R")
			}
		}
	}
	return decision{}
}

// containerValueOpts are docker/podman global options that take a separate value.
var containerValueOpts = map[string]bool{"-H": true, "--host": true, "-c": true, "--context": true, "--config": true, "-l": true, "--log-level": true, "--connection": true, "--url": true}

// privilegedRule: docker|podman [container] run|create with --privileged or a /dev mount.
func privilegedRule(c simpleCmd, _ string) decision {
	sub, rest := containerSub(c.Words)
	if (sub == "run" || sub == "create") && hostDev.MatchString(strings.Join(rest, " ")) {
		return decision{Exit: 2, Rule: "privileged-container", Msg: privilegedMsg}
	}
	return decision{}
}
