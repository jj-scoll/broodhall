package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// memdGitOrigin returns the URL `git remote get-url origin` prints in dir (insteadOf rewrites
// applied). A var so tests can simulate timeouts.
var memdGitOrigin = func(dir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), memdGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "remote", "get-url", "origin")
	cmd.Dir = dir
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return string(out), err
}

// memdProjectCache memoizes resolveProject per expanded cwd for the process lifetime. A repo
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

// memdProj is what one directory resolves to.
type memdProj struct {
	key    string // identity key, see projectKey
	legacy string // the pre-identity basename key, see legacyProjectKey
	root   string // project root the legacy key was derived from
}

// projectKey returns the project key for cwd:
//
//   - blank cwd, or a root with an empty basename ("/"): "unknown-project"
//   - "~" and "~/x" expand against $HOME
//   - with a git `origin` remote: the normalised URL (memdNormalizeRemote), e.g.
//     "github.com/jj-scoll/broodhall". Linked worktrees share the main checkout's remote, so
//     they get the same key; a submodule has its own origin and so its own key.
//   - otherwise "<basename>-<8 hex of sha256(root path)>", where root is the git work tree
//     (the main checkout for a linked worktree), else the nearest ancestor holding a
//     .memory-project / .memory.json marker (the search stops at $HOME, the temp dir and the
//     Claude config dir), else cwd itself
//
// The CLAUDE_PROJECT_DIR override is hook input normalisation, not part of the key derivation,
// and is left to callers.
func projectKey(cwd string) string { return resolveProject(cwd).key }

// legacyProjectKey is the key earlier versions wrote: the root's basename ("<parent>/<dir>"
// for a linked worktree, "<parent>/<path>" for a submodule). It is only used to find old rows.
func legacyProjectKey(cwd string) string { return resolveProject(cwd).legacy }

func resolveProject(cwd string) memdProj {
	if strings.TrimSpace(cwd) == "" {
		return memdProj{key: memdUnknownProject, legacy: memdUnknownProject}
	}
	expanded := memdExpandHome(cwd)
	if v, ok := memdProjectCache.Load(expanded); ok {
		return v.(memdProj)
	}
	gitRoot, timedOut := memdGitToplevel(expanded)
	p := memdProj{legacy: memdDeriveKey(expanded, gitRoot), root: memdProjectRoot(expanded, gitRoot)}
	p.key = p.legacy
	if p.legacy != memdUnknownProject {
		remote := ""
		if gitRoot != "" {
			if out, err := memdGitOrigin(gitRoot); err != nil {
				timedOut = timedOut || errors.Is(err, context.DeadlineExceeded)
			} else {
				remote = memdNormalizeRemote(out)
			}
		}
		if remote != "" {
			p.key = remote
		} else {
			p.key = memdFallbackKey(expanded, gitRoot)
		}
	}
	if !timedOut {
		memdProjectCache.Store(expanded, p)
	}
	return p
}

// memdProjectRoot is the directory the legacy key names: git root, else marker root, else cwd.
func memdProjectRoot(expanded, gitRoot string) string {
	if gitRoot != "" {
		return gitRoot
	}
	if r := memdMarkerRoot(expanded); r != "" {
		return r
	}
	return expanded
}

// memdFallbackKey names a project with no usable origin: basename plus a short stable hash of
// the absolute root path, so two directories called "ui" differ. A linked worktree uses the
// main checkout's path so the two agree.
func memdFallbackKey(expanded, gitRoot string) string {
	root := memdProjectRoot(expanded, gitRoot)
	gitDir := gitRoot
	if gitDir == "" {
		gitDir = expanded
	}
	if parent, name, isSub := memdLinkedRepo(gitDir); name != "" && !isSub {
		root = parent
	}
	root = memdRealpath(root)
	name := memdBasename(root)
	if name == "" {
		return memdUnknownProject
	}
	sum := sha256.Sum256([]byte(root))
	return name + "-" + hex.EncodeToString(sum[:4])
}

// memdCaseInsensitiveForges fold the whole key to lower case: these forges treat owner and
// repo names case-insensitively, so a differently-cased remote must not become a second project.
// On any other host the path keeps its case, because a self-hosted forge may distinguish
// "Team/Repo" from "team/repo". Hosts are always lower-cased.
var memdCaseInsensitiveForges = map[string]bool{"github.com": true, "gitlab.com": true, "bitbucket.org": true}

// memdNormalizeRemote reduces a git remote URL to "host/path": no scheme, credentials, port,
// trailing slash or ".git", so the ssh and https forms of one repo agree. It returns "" for
// anything without a host and a path (local paths, file:// URLs, empty input).
func memdNormalizeRemote(raw string) string {
	s := strings.TrimSpace(raw)
	var host, path string
	if i := strings.Index(s, "://"); i >= 0 {
		if strings.EqualFold(s[:i], "file") {
			return ""
		}
		rest := s[i+3:]
		slash := strings.Index(rest, "/")
		if slash < 0 {
			return ""
		}
		host, path = rest[:slash], rest[slash+1:]
	} else {
		// scp-style host:path; a "/" or "\" before the first ":" means a local path.
		colon := strings.Index(s, ":")
		if colon < 0 || strings.ContainsAny(s[:colon], `/\`) {
			return ""
		}
		host, path = s[:colon], s[colon+1:]
	}
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	if c := strings.LastIndex(host, ":"); c >= 0 && !strings.Contains(host, "]") {
		host = host[:c]
	}
	host = strings.ToLower(host)
	if len(host) < 2 { // a drive letter such as C:\src, not a host
		return ""
	}
	path = strings.Join(strings.FieldsFunc(path, func(r rune) bool { return r == '/' }), "/")
	path = strings.Trim(path, "/")
	if len(path) >= 4 && strings.EqualFold(path[len(path)-4:], ".git") {
		path = strings.Trim(path[:len(path)-4], "/")
	}
	if path == "" {
		return ""
	}
	if memdCaseInsensitiveForges[host] {
		path = strings.ToLower(path)
	}
	return host + "/" + path
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
