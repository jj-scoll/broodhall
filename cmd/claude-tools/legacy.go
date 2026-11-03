package main

// decideLegacy is the pre-tool-use rule set exactly as ~/.claude/hooks/pre-tool-use.sh ships
// it (substring blocklist, SLOW_OP warn on exit 1). It is frozen: guard-replay uses it as the
// baseline every rule change is measured against. Do not edit it to fix a rule; change
// decide() and read the replay diff instead.

import (
	"fmt"
	"regexp"
	"strings"
)

var legacyDangerous = []string{
	"rm -rf /",
	"rm -rf ~",
	"rm -rf $HOME",
	"git push --force",
	"git push -f ",
	"> /etc/",
	"dd if=",
	":(){ :|:& };:",
	"chmod -R 777 /",
	"chown -R",
}

var legacyExpensive = []string{
	"npm run build",
	"npm run test",
	"yarn build",
	"terraform apply",
	"terraform destroy",
	"docker build",
	"pytest",
	"cargo build",
	"go build",
}

var (
	containerRun = regexp.MustCompile(`(docker|podman)( container)? (run|create)`)
	hostDev      = regexp.MustCompile(`--privileged|(-v|--volume|--mount)[ =]([^ ]*source=)?/dev([:,/ ]|$)`)
)

const privilegedMsg = "BLOCKED: privileged container or host /dev mount. On this desktop it shares the real ttys and GPUs, so a systemd image grabs tty1 and kills your desktop session. For systemd-in-docker use: --tmpfs /run --tmpfs /run/lock --cgroupns=private -v /sys/fs/cgroup:/sys/fs/cgroup:rw (plus --cap-add SYS_ADMIN only if needed), and mask getty.target/systemd-logind in the image. Use a VM for anything needing real devices. Ask the user if they truly need --privileged."

// anyLine mirrors `echo "$CMD" | grep -qE re`: grep matches line by line, so `$` is a
// line end and no match spans a newline.
func anyLine(re *regexp.Regexp, s string) bool {
	for _, line := range strings.Split(s, "\n") {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

func decideLegacy(_ *guardEnv, cmd string) decision { return legacyRules(cmd, nil, false) }

// legacyRules is the shipped rule set; skip names dangerous patterns that decide() has
// replaced with a better rule, and noWarn drops the SLOW_OP warnings it replaced
// (decideLegacy skips nothing).
func legacyRules(cmd string, skip map[string]bool, noWarn bool) decision {
	// $(...) drops trailing newlines; the patterns hold no newline, so a substring test
	// equals grep's per-line one.
	cmd = strings.TrimRight(cmd, "\n")
	for _, p := range legacyDangerous {
		if skip[p] {
			continue
		}
		if strings.Contains(cmd, p) {
			return decision{Exit: 2, Rule: "dangerous:" + p,
				Msg: fmt.Sprintf("BLOCKED: '%s' is a high-risk destructive operation. Confirm with the user before attempting this.", p)}
		}
	}
	// Privileged / host-/dev containers (a systemd image took over tty1 and killed the desktop session,
	// 2026-10-07). Matches the command line only; a script that runs
	// `docker run --privileged` internally slips past.
	if anyLine(containerRun, cmd) && anyLine(hostDev, cmd) {
		return decision{Exit: 2, Rule: "privileged-container", Msg: privilegedMsg}
	}
	for _, p := range legacyExpensive {
		if !noWarn && strings.Contains(cmd, p) {
			return decision{Exit: 1, Rule: "slow-op",
				Msg: fmt.Sprintf("SLOW_OP: '%s' detected. Only run this if there is a concrete reason (e.g. verifying a build or running tests). If this is exploratory, use a cheaper alternative first.", p)}
		}
	}
	return decision{}
}
