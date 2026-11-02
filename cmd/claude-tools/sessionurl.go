package main

// Session-URL egress gate. The rule is that a claude.ai session link or a
// Claude-Session trailer never leaves the machine, yet the harness's own attribution
// reminder tells sessions to add one (31 commits on one repo's main branch carried it). The
// gate sits at the boundary instead of at authoring time, so it catches every path
// (heredocs, -F files, editors, subagents): it blocks `git push` when an unpushed commit
// message carries a session link, and `gh pr create|edit` when the command text or a
// --body-file does.
//
// The pattern needs a real-looking ID: 128 Write/Edit calls (mostly HANDOFF.md) contain
// the rule text "never write a Claude-Session: line" or "claude.ai/code/session_<id>", and
// a phrase match would block them. Real IDs in the history are 24 alphanumerics after
// "session_"; the pattern asks for 16 or more.
//
// Limits: only Bash is covered (the GitHub MCP create_pull_request tool is not), a push
// hidden in `bash -c` is not parsed, commits already on a remote ref are not rechecked
// (rewriting shared history is a separate decision), and cloud sessions run no user hooks.

import (
	"context"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var sessionRe = regexp.MustCompile(`claude\.ai/code/session_[A-Za-z0-9]{16,}`)

// gitUnpushed lists the messages of commits in dir that no remote ref contains, each led
// by \x01 and its short hash. Any failure (not a repository, no git, timeout) gives "":
// the push would fail on its own.
func gitUnpushed(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "log", "-n", "200", "--format=%x01%h%n%B", "HEAD", "--not", "--remotes")
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

var (
	pushFallback = regexp.MustCompile(`(?m)(?:^|[;&|(]\s*)git\s+(?:-\S+\s+)*push\b|(?:^|[;&|(]\s*)gh\s+(?:-\S+\s+)*pr\s+(?:create|edit)\b`)
	ghFileFlag   = regexp.MustCompile(`^(?:--body-file|-F)(?:=(.*))?$`)
)

const sessionURLFix = " Never add the trailer (CLAUDE.md safety boundary): the harness attribution reminder is wrong here."

func sessionURLBlock(msg string) decision {
	return decision{Exit: 2, Rule: "session-url", Msg: "BLOCKED: " + msg}
}

func sessionURLRule(env *guardEnv, cmd string) decision {
	var pushDirs, bodyFiles []string
	egress := false
	// Detect the egress commands on heredoc-free text so a body with apostrophes still
	// parses; the scan below uses the raw command, which holds the body.
	segs, ok := shellSegments(stripHeredocs(cmd))
	if !ok && pushFallback.MatchString(cmd) {
		egress = true
		pushDirs = append(pushDirs, env.Cwd)
	}
	dir := env.Cwd
	for _, seg := range segs {
		seg = peelWrappers(seg)
		if len(seg) == 0 {
			continue
		}
		args := seg[1:]
		switch path.Base(seg[0]) {
		case "cd":
			if len(args) > 0 {
				dir = resolveDir(dir, args[0], env.Home)
			}
		case "git":
			gdir := dir
			i := 0
			for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
				switch args[i] {
				case "-C":
					if i+1 < len(args) {
						gdir = resolveDir(gdir, args[i+1], env.Home)
						i++
					}
				case "-c", "--git-dir", "--work-tree", "--namespace":
					i++
				}
			}
			if i < len(args) && args[i] == "push" {
				egress = true
				pushDirs = append(pushDirs, gdir)
			}
		case "gh":
			i := 0
			for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
				if args[i] == "-R" || args[i] == "--repo" {
					i++
				}
			}
			if i+1 < len(args) && args[i] == "pr" && (args[i+1] == "create" || args[i+1] == "edit") {
				egress = true
				for j := i + 2; j < len(args); j++ {
					m := ghFileFlag.FindStringSubmatch(args[j])
					switch {
					case m == nil:
					case strings.Contains(args[j], "="):
						bodyFiles = append(bodyFiles, resolveDir(dir, m[1], env.Home))
					case j+1 < len(args) && args[j+1] != "-":
						bodyFiles = append(bodyFiles, resolveDir(dir, args[j+1], env.Home))
					}
				}
			}
		}
	}
	if !egress {
		return decision{}
	}
	if sessionRe.MatchString(cmd) {
		return sessionURLBlock("this command carries a claude.ai session link or Claude-Session trailer (in the command text or body) and would send it off this machine. Remove it and run again." + sessionURLFix)
	}
	for _, f := range bodyFiles {
		if b, err := os.ReadFile(filepath.Clean(f)); err == nil && len(b) < 4<<20 && sessionRe.Match(b) {
			return sessionURLBlock("the body file " + f + " carries a claude.ai session link or Claude-Session trailer. Remove it and run again." + sessionURLFix)
		}
	}
	if env.GitUnpushed == nil {
		return decision{}
	}
	for _, d := range pushDirs {
		var hashes []string
		for _, chunk := range strings.Split(env.GitUnpushed(d), "\x01") {
			if sessionRe.MatchString(chunk) {
				h, _, _ := strings.Cut(chunk, "\n")
				hashes = append(hashes, h)
			}
		}
		if len(hashes) > 0 {
			return sessionURLBlock("unpushed commit(s) " + strings.Join(hashes, ", ") + " carry a claude.ai session link / Claude-Session trailer. Rewrite them before pushing: `git commit --amend` for HEAD, or `git rebase --exec` / `git filter-branch --msg-filter` for older ones (git rebase -i is unavailable here)." + sessionURLFix)
		}
	}
	return decision{}
}
