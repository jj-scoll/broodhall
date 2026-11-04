package main

// Sweep guard: several Claude Code sessions often work in one checkout. A sweeping git
// command (git add -A, git commit -a, git stash, git checkout ., git restore ., git reset
// --hard, git clean -f) takes in every uncommitted change in the tree, including a line a
// different, live session just wrote. This rule asks first, and names that session and its
// files.
//
// Ownership comes from record-edit (editrecord.go): the paths each session wrote with the
// Write, Edit, MultiEdit and NotebookEdit tools. Only a file recorded for another live session
// that is dirty right now, and that the command would really take in, counts. Changes made
// through Bash (sed -i, a redirect, a formatter) are not recorded, so they never count.
//
// A linked worktree is another checkout: paths are absolute, and git status runs in the
// command's own repo, so another worktree's files never match. Cheap path first: no other live
// session with recorded edits means no git process runs. Otherwise two git calls, bounded by a
// timeout, the status limited to the files the other sessions edited. Anything the guard
// cannot establish (no repo, a timeout, a command that retargets git) lets the command through.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	sweepRule     = "shared-tree-sweep"
	sweepMaxFiles = 5
	sweepMaxPaths = 300 // above this many recorded paths git status runs unlimited
	sweepTimeout  = 1500 * time.Millisecond
)

// statusEntry is one line of git status --porcelain: the XY codes and the path relative to
// the repository root.
type statusEntry struct{ XY, Path string }

func (e statusEntry) untracked() bool { return e.XY == "??" }
func (e statusEntry) tracked() bool   { return !e.untracked() && e.XY != "!!" }

// worktreeDirty: a tracked file whose working copy differs from the index.
func (e statusEntry) worktreeDirty() bool { return e.tracked() && e.XY[1] != ' ' }

// sweepCmd is one sweeping git invocation: what it is called, where git runs, which
// pathspecs limit it (nil: the whole tree) and which status entries it would take in.
type sweepCmd struct {
	Label string
	Dir   string
	Scope []string
	Takes func(statusEntry) bool
}

// gitArgs is a git subcommand's arguments sorted into short-option letters, long options and
// positionals (pathspecs, revisions).
type gitArgs struct {
	short string
	long  map[string]bool
	pos   []string
}

func (g gitArgs) has(letter byte, long string) bool {
	return strings.IndexByte(g.short, letter) >= 0 || g.long[long]
}

// parseGitArgs splits args. valued are the short letters that take a value (the rest of the
// cluster, or the next word); longValued the long options that take the next word when they
// have no "=". Everything after "--" is positional.
func parseGitArgs(args []string, valued string, longValued ...string) gitArgs {
	g := gitArgs{long: map[string]bool{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			g.pos = append(g.pos, args[i+1:]...)
			return g
		case strings.HasPrefix(a, "--"):
			name, _, hasVal := strings.Cut(a[2:], "=")
			g.long[name] = true
			if !hasVal {
				for _, lv := range longValued {
					if lv == name && i+1 < len(args) {
						i++
					}
				}
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			for j := 1; j < len(a); j++ {
				g.short += string(a[j])
				if strings.IndexByte(valued, a[j]) >= 0 {
					if j == len(a)-1 && i+1 < len(args) {
						i++
					}
					break
				}
			}
		default:
			g.pos = append(g.pos, a)
		}
	}
	return g
}

// wholePathspec: a pathspec that names the current directory and everything under it.
func wholePathspec(p string) bool {
	if p == "*" || p == ":/" {
		return true
	}
	if p == "" || strings.HasPrefix(p, ":") || strings.HasPrefix(p, "/") {
		return false
	}
	for _, seg := range strings.Split(path.Clean(p), "/") {
		if seg != "." && seg != ".." {
			return false
		}
	}
	return true
}

// gitWords returns the directory git runs in, or ok=false when the command points git
// somewhere this rule cannot follow (--git-dir, --work-tree, GIT_DIR, GIT_WORK_TREE).
func gitWords(c simpleCmd, dir, home string) (string, bool) {
	for _, e := range c.Env {
		if strings.HasPrefix(e, "GIT_DIR=") || strings.HasPrefix(e, "GIT_WORK_TREE=") {
			return dir, false
		}
	}
	for i := 1; i < len(c.Words) && strings.HasPrefix(c.Words[i], "-"); i++ {
		switch a := c.Words[i]; {
		case a == "-C" && i+1 < len(c.Words):
			dir = resolveDir(dir, c.Words[i+1], home)
			i++
		case a == "--git-dir" || a == "--work-tree" || strings.HasPrefix(a, "--git-dir=") || strings.HasPrefix(a, "--work-tree="):
			return dir, false
		case gitArgOpts[a]:
			i++
		}
	}
	return dir, true
}

// findSweeps lists the sweeping git invocations in cmds, tracking `cd` between them.
func findSweeps(cmds []simpleCmd, cwd, home string) []sweepCmd {
	var out []sweepCmd
	dir := cwd
	for _, c := range cmds {
		if len(c.Words) == 0 {
			continue
		}
		if path.Base(c.Words[0]) == "cd" {
			if len(c.Words) > 1 {
				dir = resolveDir(dir, c.Words[1], home)
			}
			continue
		}
		sub, args := gitSub(c.Words)
		if sub == "" {
			continue
		}
		gdir, ok := gitWords(c, dir, home)
		if !ok {
			continue
		}
		if s, ok := sweepOf(sub, args); ok {
			s.Dir = gdir
			out = append(out, s)
		}
	}
	return out
}

func trackedAny(e statusEntry) bool { return e.tracked() }

// sweepOf says whether git <sub> <args> sweeps, and what it takes in.
func sweepOf(sub string, args []string) (sweepCmd, bool) {
	switch sub {
	case "add":
		g := parseGitArgs(args, "", "pathspec-from-file", "chmod")
		if g.has('n', "dry-run") {
			return sweepCmd{}, false
		}
		whole := false
		for _, p := range g.pos {
			whole = whole || wholePathspec(p)
		}
		all := g.has('A', "all") || g.has('A', "no-ignore-removal")
		if !all && !g.has('u', "update") && !whole {
			return sweepCmd{}, false
		}
		s := sweepCmd{Label: "git add", Scope: g.pos}
		switch {
		case g.long["pathspec-from-file"]:
			s.Scope = nil
		case all:
			s.Label = "git add -A"
		case g.has('u', "update"):
			s.Label = "git add -u"
			s.Takes = func(e statusEntry) bool { return e.worktreeDirty() }
			return s, true
		default:
			s.Label = "git add ."
		}
		s.Takes = func(e statusEntry) bool { return e.untracked() || e.worktreeDirty() }
		return s, true
	case "commit":
		g := parseGitArgs(args, "mFCct", "message", "file", "author", "date", "template", "cleanup", "fixup", "squash", "reuse-message", "reedit-message", "trailer", "pathspec-from-file")
		if !g.has('a', "all") || g.long["dry-run"] {
			return sweepCmd{}, false
		}
		return sweepCmd{Label: "git commit -a", Takes: func(e statusEntry) bool { return e.worktreeDirty() }}, true
	case "stash":
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") && args[0] != "push" && args[0] != "save" {
			return sweepCmd{}, false // list, show, pop, apply, drop, clear, branch, create, store
		}
		save := len(args) > 0 && args[0] == "save"
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			args = args[1:]
		}
		g := parseGitArgs(args, "m", "message")
		s := sweepCmd{Label: "git stash"}
		if !save {
			s.Scope = g.pos
		}
		untracked := g.has('u', "include-untracked") || g.has('a', "all")
		s.Takes = func(e statusEntry) bool { return trackedAny(e) || (untracked && e.untracked()) }
		return s, true
	case "switch":
		g := parseGitArgs(args, "cCt", "create", "force-create", "track", "orphan")
		if !g.has('f', "force") && !g.long["discard-changes"] {
			return sweepCmd{}, false
		}
		return sweepCmd{Label: "git switch -f", Takes: trackedAny}, true
	case "checkout", "restore":
		g := parseGitArgs(args, "sbBU", "source", "pathspec-from-file")
		dot := false
		for _, p := range g.pos {
			dot = dot || p == "."
		}
		if !dot && sub == "checkout" && g.has('f', "force") && len(g.pos) <= 1 && !hasArg(args, "--") {
			return sweepCmd{Label: "git checkout -f", Takes: trackedAny}, true // replaces every tracked file
		}
		if !dot {
			return sweepCmd{}, false
		}
		s := sweepCmd{Label: "git " + sub + " .", Scope: []string{"."}, Takes: func(e statusEntry) bool { return e.worktreeDirty() }}
		if sub == "restore" {
			if g.has('S', "staged") && !g.has('W', "worktree") {
				return sweepCmd{}, false // only unstages
			}
			if g.has('S', "staged") || g.has('s', "source") {
				s.Takes = trackedAny
			}
		} else if len(g.pos) > 1 {
			s.Takes = trackedAny // git checkout <tree-ish> . also replaces the index
		}
		return s, true
	case "reset":
		if !hasArg(args, "--hard") {
			return sweepCmd{}, false
		}
		return sweepCmd{Label: "git reset --hard", Takes: trackedAny}, true
	case "clean":
		g := parseGitArgs(args, "e", "exclude")
		if !g.has('f', "force") || g.has('n', "dry-run") {
			return sweepCmd{}, false
		}
		s := sweepCmd{Label: "git clean -f", Scope: g.pos, Takes: func(e statusEntry) bool { return e.untracked() }}
		if len(s.Scope) == 0 {
			s.Scope = []string{"."}
		}
		return s, true
	}
	return sweepCmd{}, false
}

// inScope reports whether repo-relative path p falls under the pathspecs, which are
// resolved against dir. Magic and glob pathspecs count as the whole tree: asking too often
// is the safe error.
func inScope(scope []string, dir, top, home string) func(p string) bool {
	if scope == nil {
		return func(string) bool { return true }
	}
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	var rels []string
	for _, s := range scope {
		if strings.HasPrefix(s, ":") || strings.ContainsAny(s, "*?[") && !wholePathspec(s) {
			return func(string) bool { return true }
		}
		if s == "*" {
			s = "."
		}
		rel, err := filepath.Rel(top, resolveDir(dir, s, home))
		switch {
		case err != nil || rel == ".." || strings.HasPrefix(rel, "../"):
			// outside this repository: matches nothing here
		case rel == ".":
			return func(string) bool { return true }
		default:
			rels = append(rels, filepath.ToSlash(rel))
		}
	}
	return func(p string) bool {
		for _, r := range rels {
			if p == r || strings.HasPrefix(p, r+"/") {
				return true
			}
		}
		return false
	}
}

// sweepGuard asks when a sweeping command would take in a file another live session edited.
func sweepGuard(env *guardEnv, cmds []simpleCmd) decision {
	if env.OtherEdits == nil || env.GitStatus == nil {
		return decision{}
	}
	sweeps := findSweeps(cmds, env.Cwd, env.Home)
	if len(sweeps) == 0 {
		return decision{}
	}
	others := env.OtherEdits(env.Session, env.Now)
	if len(others) == 0 {
		return decision{}
	}
	sort.Slice(others, func(i, j int) bool { return others[i].Last.After(others[j].Last) })
	seen := map[string]bool{}
	var cands []string
	for _, o := range others {
		for f := range o.Files {
			if !seen[f] {
				seen[f] = true
				cands = append(cands, f)
			}
		}
	}
	sort.Strings(cands)
	type statusResult struct {
		top  string
		ents []statusEntry
	}
	cache := map[string]statusResult{}
	for _, s := range sweeps {
		r, ok := cache[s.Dir]
		if !ok {
			r.top, r.ents = env.GitStatus(s.Dir, cands)
			cache[s.Dir] = r
		}
		if r.top == "" {
			continue
		}
		in := inScope(s.Scope, s.Dir, r.top, env.Home)
		byOwner := map[string][]string{}
		for _, e := range r.ents {
			if !s.Takes(e) || !in(e.Path) {
				continue
			}
			abs := filepath.Join(r.top, e.Path)
			for _, o := range others {
				if o.Files[abs] {
					byOwner[o.ID] = append(byOwner[o.ID], e.Path)
				}
			}
		}
		if len(byOwner) == 0 {
			continue
		}
		var parts []string
		listed, total := 0, 0
		for _, o := range others {
			files := byOwner[o.ID]
			if len(files) == 0 {
				continue
			}
			sort.Strings(files)
			total += len(files)
			var shown []string
			for _, f := range files {
				if listed < sweepMaxFiles {
					shown = append(shown, printable(f))
					listed++
				}
			}
			if len(shown) > 0 {
				parts = append(parts, fmt.Sprintf("session %s (active %s): %s", shortID(o.ID), ago(env.Now.Sub(o.Last)), strings.Join(shown, ", ")))
			}
		}
		more := ""
		if total > listed {
			more = fmt.Sprintf(" (+%d more)", total-listed)
		}
		return decision{Ask: true, Rule: sweepRule,
			Msg: "ASK: " + s.Label + " would take in uncommitted changes that another live Claude session made in this checkout: " +
				strings.Join(parts, "; ") + more + ". Confirm with the user before running it; stage or stash only your own files by path instead."}
	}
	return decision{}
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func ago(d time.Duration) string {
	if d < time.Minute {
		return "under a minute ago"
	}
	return fmt.Sprintf("%d min ago", int(d/time.Minute))
}

// gitStatusOf finds the repository root for dir and runs git status limited to files (the
// absolute paths other sessions edited) that lie under it. The root comes back with the
// dirty entries; "" means no repository, a timeout, or no file of interest. Both git calls
// share one short timeout, take no optional locks and treat pathspecs literally.
func gitStatusOf(dir string, files []string) (string, []statusEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), sweepTimeout)
	defer cancel()
	run := func(wd string, args ...string) ([]byte, error) {
		c := exec.CommandContext(ctx, "git", args...)
		c.Dir = wd
		c.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
		c.WaitDelay = 200 * time.Millisecond
		return c.Output()
	}
	out, err := run(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", nil
	}
	top := strings.TrimRight(string(out), "\n")
	if r, err := filepath.EvalSymlinks(top); err == nil {
		top = r
	}
	args := []string{"--literal-pathspecs", "status", "--porcelain=v1", "-z", "-uall", "--"}
	var rels []string
	for _, f := range files {
		if rel, err := filepath.Rel(top, f); err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, "../") {
			rels = append(rels, rel)
		}
	}
	if len(rels) == 0 {
		return "", nil
	}
	if len(rels) <= sweepMaxPaths { // more than that: the whole status is cheaper than the long argv
		args = append(args, rels...)
	}
	out, err = run(top, args...)
	if err != nil {
		return "", nil
	}
	return top, parseStatusZ(out)
}

// parseStatusZ reads git status --porcelain=v1 -z: "XY path" entries, with the old name of a
// rename or copy as an extra field.
func parseStatusZ(b []byte) []statusEntry {
	var ents []statusEntry
	f := strings.Split(string(b), "\x00")
	for i := 0; i < len(f); i++ {
		if len(f[i]) < 4 || f[i][2] != ' ' {
			continue
		}
		xy := f[i][:2]
		ents = append(ents, statusEntry{xy, f[i][3:]})
		if xy[0] == 'R' || xy[0] == 'C' || xy[1] == 'R' || xy[1] == 'C' {
			i++
		}
	}
	return ents
}

// printable quotes a file name that holds a control character, so it cannot break the line
// or add text of its own to the reason.
func printable(s string) string {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return strconv.Quote(s)
		}
	}
	return s
}
