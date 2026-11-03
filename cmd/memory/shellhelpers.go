package main

import (
	"path"
	"regexp"
	"strings"
)

// containerValueOpts are docker/podman global options that take a separate value.
var containerValueOpts = map[string]bool{"-H": true, "--host": true, "-c": true, "--context": true, "--config": true, "-l": true, "--log-level": true, "--connection": true, "--url": true}

// camelRE splits camelCase key names at the lower-to-upper boundary.
var camelRE = regexp.MustCompile(`([a-z0-9])([A-Z])`)

// wrapperArgFlags are the sudo flags that take a separate argument.
var wrapperArgFlags = map[string]bool{"-u": true, "-g": true, "-h": true, "-p": true, "-C": true, "-D": true, "-R": true, "-T": true, "-U": true}

// peelWrappers drops leading assignments and wrappers (sudo -u x, env A=b, command, nohup,
// ...) so w[0] is the command that actually runs.
func peelWrappers(w []string) []string {
	for len(w) > 0 {
		switch {
		case strings.Contains(w[0], "=") && !strings.HasPrefix(w[0], "-") && !strings.Contains(w[0], "/"):
			w = w[1:]
		case path.Base(w[0]) == "sudo" || path.Base(w[0]) == "doas" || w[0] == "env" || w[0] == "command" || w[0] == "nohup" || w[0] == "exec" || w[0] == "time":
			isSudo := path.Base(w[0]) == "sudo"
			w = w[1:]
			for len(w) > 0 && strings.HasPrefix(w[0], "-") && w[0] != "-" {
				skip := 1
				if isSudo && wrapperArgFlags[w[0]] {
					skip = 2
				}
				if skip > len(w) {
					skip = len(w)
				}
				w = w[skip:]
			}
		default:
			return w
		}
	}
	return w
}
