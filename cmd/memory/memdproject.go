package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// memdGitTimeout bounds one `git rev-parse` so a hung git cannot stall a hook.
const memdGitTimeout = 2 * time.Second

// memdGitRunner returns `git rev-parse --show-toplevel` output for dir. A var so tests can
// count calls and simulate timeouts.
var memdGitRunner = func(dir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), memdGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return string(out), err
}

// memdProjectCache memoizes projectKey per expanded cwd for the process lifetime. A repo
// moved or renamed under a running daemon keeps its old key until restart.
var memdProjectCache sync.Map

// memdUnknownProject is the fallback name for an empty cwd or a bare filesystem root.
const memdUnknownProject = "unknown-project"

// memdProjectMarkers are the files whose presence in an ancestor directory pins the project
// root for non-git trees.
var memdProjectMarkers = []string{".memory-project", ".memory.json"}

var (
	reWorktreeGitdir = regexp.MustCompile(`^(.+)/\.git/worktrees/([^/]+)$`)
	reSubmoduleDir   = regexp.MustCompile(`^(.+)/\.git/modules/(.+)$`)
	reBareWorktree   = regexp.MustCompile(`^(.+)/worktrees/([^/]+)$`)
	reGitdirLine     = regexp.MustCompile(`^gitdir:\s*(.+)$`)
)

// projectKey returns the project name for cwd (path-based naming):
//
//   - blank cwd, or a root with an empty basename ("/"): "unknown-project"
//   - "~" and "~/x" expand against $HOME
//   - the root is `git rev-parse --show-toplevel`, else the nearest ancestor holding a
//     .memory-project / .memory.json marker (the search stops at $HOME, the temp dir
//     and the Claude config dir), else cwd itself
//   - the name is the root's basename; for a linked git worktree it is "<parent>/<dir>" (just
//     "<parent>" when the two are equal), for a submodule "<parent>/<path inside parent>"
//
// The CLAUDE_PROJECT_DIR override is hook input normalisation, not part of the name derivation,
// and is left to callers.
func projectKey(cwd string) string {
	if strings.TrimSpace(cwd) == "" {
		return memdUnknownProject
	}
	expanded := memdExpandHome(cwd)
	if v, ok := memdProjectCache.Load(expanded); ok {
		return v.(string)
	}
	gitRoot, timedOut := memdGitToplevel(expanded)
	key := memdDeriveKey(expanded, gitRoot)
	if !timedOut {
		memdProjectCache.Store(expanded, key)
	}
	return key
}

func memdDeriveKey(expanded, gitRoot string) string {
	root := gitRoot
	if root == "" {
		root = memdMarkerRoot(expanded)
	}
	if root == "" {
		root = expanded
	}
	name := memdBasename(root)
	if name == "" {
		return memdUnknownProject
	}

	// Worktree and submodule detection reads <git root or cwd>/.git; markers are not consulted.
	gitDir := gitRoot
	if gitDir == "" {
		gitDir = expanded
	}
	parentRepo, parentName, isSub := memdLinkedRepo(gitDir)
	switch {
	case parentName == "":
		return name
	case isSub:
		if rel, err := filepath.Rel(parentRepo, gitDir); err == nil && rel != "." && rel != "" &&
			!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
			return parentName + "/" + filepath.ToSlash(rel)
		}
		return parentName + "/" + name
	case parentName == name:
		return name
	default:
		return parentName + "/" + name
	}
}

// memdBasename is Node's path.basename: trailing slashes ignored, "" for "/".
func memdBasename(p string) string {
	p = strings.TrimRight(p, "/")
	if p == "" {
		return ""
	}
	return filepath.Base(p)
}

func memdExpandHome(p string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// memdGitToplevel returns the git work-tree root containing dir, or "" when dir is not in one
// (or does not exist). timedOut reports that git exceeded memdGitTimeout; callers must not
// cache such a result.
func memdGitToplevel(dir string) (root string, timedOut bool) {
	out, err := memdGitRunner(dir)
	if err != nil {
		return "", errors.Is(err, context.DeadlineExceeded)
	}
	return strings.TrimSpace(out), false
}

// memdMarkerRoot walks up from dir (at most 64 levels) looking for a project marker file. It
// returns "" when dir is inside the Claude config dir, when it reaches $HOME / the temp dir /
// the config dir / the filesystem root first, or when no marker exists.
func memdMarkerRoot(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	cfg := os.Getenv("CLAUDE_CONFIG_DIR")
	home, _ := os.UserHomeDir()
	if cfg == "" && home != "" {
		cfg = filepath.Join(home, ".claude")
	}
	var stops []string
	for _, s := range []string{home, os.TempDir(), cfg} {
		if s == "" {
			continue
		}
		s = filepath.Clean(s)
		stops = append(stops, s)
		if real, err := filepath.EvalSymlinks(s); err == nil {
			stops = append(stops, real)
		}
	}
	if cfg != "" {
		for _, c := range []string{filepath.Clean(cfg), memdRealpath(cfg)} {
			if rel, err := filepath.Rel(c, abs); err == nil && !strings.HasPrefix(rel, "..") {
				return ""
			}
		}
	}
	r := abs
	for i := 0; i < 64; i++ {
		for _, s := range stops {
			if r == s {
				return ""
			}
		}
		parent := filepath.Dir(r)
		if parent == r {
			return ""
		}
		for _, m := range memdProjectMarkers {
			if _, err := os.Stat(filepath.Join(r, m)); err == nil {
				return r
			}
		}
		r = parent
	}
	return ""
}

func memdRealpath(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return p
}

// memdLinkedRepo inspects dir/.git. When it is a file pointing at a linked worktree or a
// submodule it returns the parent repo path and its project name; otherwise parentName is "".
func memdLinkedRepo(dir string) (parentRepo, parentName string, isSub bool) {
	gitFile := filepath.Join(dir, ".git")
	st, err := os.Stat(gitFile)
	if err != nil || !st.Mode().IsRegular() {
		return "", "", false
	}
	b, err := os.ReadFile(gitFile)
	if err != nil {
		return "", "", false
	}
	m := reGitdirLine.FindStringSubmatch(strings.TrimSpace(string(b)))
	if m == nil {
		return "", "", false
	}
	gd := m[1]
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(dir, gd)
	}
	gd = filepath.ToSlash(filepath.Clean(gd))
	if s := reWorktreeGitdir.FindStringSubmatch(gd); s != nil {
		return s[1], memdBasename(s[1]), false
	}
	if s := reSubmoduleDir.FindStringSubmatch(gd); s != nil {
		return s[1], memdBasename(s[1]), true
	}
	if s := reBareWorktree.FindStringSubmatch(gd); s != nil {
		if _, err := os.Stat(filepath.Join(gd, "commondir")); err == nil {
			if n := memdRepoName(s[1]); n != "" {
				return s[1], n, false
			}
		}
	}
	return "", "", false
}

// memdRepoName names a bare/common repo path: basename without ".git", else (for dot-names such
// as ".git") the parent directory's basename.
func memdRepoName(p string) string {
	n := strings.TrimSuffix(memdBasename(p), ".git")
	if n != "" && !strings.HasPrefix(n, ".") {
		return n
	}
	return memdBasename(filepath.Dir(p))
}
