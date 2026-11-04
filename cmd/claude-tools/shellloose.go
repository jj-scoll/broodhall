package main

// looseDestructive is the net under the parser: text mvdan cannot read (zsh-only syntax such
// as ${(f)x} or =(cmd), a broken quote, an inner script that does not parse) still gets its
// rm and force-push rules, but read as plain words, so a spelling the legacy substrings miss
// (rm -fr /, rm  -rf  /, git push  -f) is caught. It errs toward asking, never toward
// blocking: the text may not be what a shell would run.

import (
	"path"
	"strings"
)

func looseDestructive(cmd, home string) string {
	cmd = strings.NewReplacer(`"`, " ", `'`, " ").Replace(cmd)
	for _, seg := range segSplit.Split(cmd, -1) {
		w := peelWrappers(strings.Fields(seg))
		if len(w) == 0 {
			continue
		}
		if path.Base(w[0]) == "rm" {
			if t := rmArgsTarget(w[1:], home); t != "" {
				return "rm -rf " + t + " in text the shell parser could not read"
			}
		}
		if f := gitPushForce(w); f != "" {
			return "git push " + f + " in text the shell parser could not read"
		}
	}
	return ""
}
