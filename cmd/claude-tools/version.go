package main

// version: prints the release this binary was built from and its commit, such as
// "0.2.0 (5b8331f)", plus "-dirty" on the commit when the tree had uncommitted changes.
// Release builds stamp both with -ldflags (see .github/workflows/release.yml); a plain
// `go build` in a checkout carries the commit in its VCS info, and `go install ...@v0.2.0`
// carries the release in the module version. "unknown" means none of those was available.

import (
	"fmt"
	"regexp"
	"runtime/debug"
	"strings"
)

func versionCmd(args []string) int {
	bi, ok := debug.ReadBuildInfo()
	rev, dirty := buildRev(bi, ok)
	fmt.Println(versionString(buildVersion(bi, ok), rev, dirty))
	return 0
}

// stampRev is the source commit and stampVersion the release tag for builds without VCS or
// module info, set with -ldflags "-X main.stampRev=<sha> -X main.stampVersion=<tag>".
var (
	stampRev     string
	stampVersion string
)

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

// pseudoVersion matches the module version Go gives an untagged commit, which is not a release.
var pseudoVersion = regexp.MustCompile(`[-.][0-9]{14}-[0-9a-f]{12}(\+dirty)?$`)

// buildVersion is the release number without its leading v: the stamped tag, else the main
// module's version when it is a tagged release, else "".
func buildVersion(bi *debug.BuildInfo, ok bool) string {
	if stampVersion != "" {
		return strings.TrimPrefix(stampVersion, "v")
	}
	if !ok {
		return ""
	}
	v := bi.Main.Version
	if v == "" || v == "(devel)" || pseudoVersion.MatchString(v) {
		return ""
	}
	return strings.TrimPrefix(v, "v")
}

func showRev(rev string, dirty bool) string {
	if rev == "" {
		return ""
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}

// versionString joins the release and the commit; "unknown" when there is neither.
func versionString(ver, rev string, dirty bool) string {
	r := showRev(rev, dirty)
	switch {
	case ver != "" && r != "":
		return ver + " (" + r + ")"
	case ver != "":
		return ver
	case r != "":
		return r
	}
	return "unknown"
}
