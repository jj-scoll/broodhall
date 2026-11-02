package main

// version: prints the commit this binary was built from, plus "-dirty" when the tree had
// uncommitted changes. `go build` in a git checkout stamps it in; patchelf (build.sh) makes
// `go version -m` unable to read it from outside, but the binary still reads its own.

import (
	"fmt"
	"runtime/debug"
)

func versionCmd(args []string) int {
	rev, dirty := buildRev(debug.ReadBuildInfo())
	fmt.Println(showRev(rev, dirty))
	return 0
}

// stampRev is the source commit for builds without VCS info (the Nix package, built
// from a tree with no .git), set with -ldflags "-X main.stampRev=<rev>".
var stampRev string

func buildRev(bi *debug.BuildInfo, ok bool) (rev string, dirty bool) {
	if !ok {
		return stampRev, false
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return stampRev, false
	}
	return rev, dirty
}

func showRev(rev string, dirty bool) string {
	if rev == "" {
		return "unknown"
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}
