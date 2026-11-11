package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
)

func TestNormalizeRemote(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"https with .git", "https://github.com/jj-scoll/broodhall.git", "github.com/jj-scoll/broodhall"},
		{"https without .git", "https://github.com/jj-scoll/broodhall", "github.com/jj-scoll/broodhall"},
		{"scp-style", "git@github.com:jj-scoll/broodhall.git", "github.com/jj-scoll/broodhall"},
		{"scp-style without user", "github.com:jj-scoll/broodhall", "github.com/jj-scoll/broodhall"},
		{"ssh scheme", "ssh://git@github.com/jj-scoll/broodhall.git", "github.com/jj-scoll/broodhall"},
		{"ssh scheme with port", "ssh://git@github.com:2222/jj-scoll/broodhall.git", "github.com/jj-scoll/broodhall"},
		{"https credentials and port", "https://user:tok@github.com:443/jj-scoll/broodhall/", "github.com/jj-scoll/broodhall"},
		{"token-only credentials", "https://tok@github.com/jj-scoll/broodhall", "github.com/jj-scoll/broodhall"},
		{"trailing slash after .git", "https://github.com/jj-scoll/broodhall.git/", "github.com/jj-scoll/broodhall"},
		{"git protocol", "git://github.com/jj-scoll/broodhall.git", "github.com/jj-scoll/broodhall"},
		{"mixed case on github folds the path", "HTTPS://GitHub.COM/JJ-Scoll/Broodhall.GIT", "github.com/jj-scoll/broodhall"},
		{"mixed case on gitlab folds the path", "https://GitLab.com/Grp/Sub/Repo.git", "gitlab.com/grp/sub/repo"},
		{"unknown forge keeps path case", "https://Git.Example.com/Team/Repo.git", "git.example.com/Team/Repo"},
		{"unknown forge ssh and https agree", "git@git.example.com:Team/Repo.git", "git.example.com/Team/Repo"},
		{"nested groups", "https://gitlab.com/g/sub/r.git", "gitlab.com/g/sub/r"},
		{"surrounding whitespace", " https://github.com/o/r.git\n", "github.com/o/r"},
		{"doubled slashes", "https://github.com//o//r.git", "github.com/o/r"},
		{"only one .git is trimmed", "https://github.com/o/r.git.git", "github.com/o/r.git"},
		{"empty", "", ""},
		{"absolute path", "/srv/git/r.git", ""},
		{"relative path", "../r", ""},
		{"file scheme", "file:///srv/git/r.git", ""},
		{"windows drive path", `C:\src\r`, ""},
		{"host without path", "https://github.com", ""},
		{"host with slash only", "https://github.com/", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := memdNormalizeRemote(tt.in); got != tt.want {
				t.Errorf("memdNormalizeRemote(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

var reHashSuffix = regexp.MustCompile(`^[^/]+-[0-9a-f]{8}$`)

func TestProjectKeyIdentity(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "cfg"))
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("HOME", filepath.Join(root, "home"))

	newRepo := func(name, origin string) string {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		gitRun(t, dir, "init", "-q")
		gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
		if origin != "" {
			gitRun(t, dir, "remote", "add", "origin", origin)
		}
		return dir
	}

	a := newRepo("a-clone", "https://github.com/jj-scoll/broodhall.git")
	b := newRepo("b-clone", "git@github.com:JJ-Scoll/Broodhall.git")
	if err := os.MkdirAll(filepath.Join(a, "pkg", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(root, "wt-feature")
	gitRun(t, a, "worktree", "add", "-q", "-b", "feature", wt)

	noRemote := newRepo("lonely", "")
	noRemoteWT := filepath.Join(root, "lonely-wt")
	gitRun(t, noRemote, "worktree", "add", "-q", "-b", "side", noRemoteWT)
	localOrigin := newRepo("localorigin", "/srv/git/x.git")

	// submodule: a real one, whose own origin is repointed at a URL.
	lib := newRepo("lib-src", "")
	super := newRepo("super", "https://github.com/jj-scoll/super.git")
	gitRun(t, super, "-c", "protocol.file.allow=always", "submodule", "add", "-q", lib, "libs/child")
	sub := filepath.Join(super, "libs", "child")
	gitRun(t, sub, "remote", "set-url", "origin", "https://github.com/jj-scoll/child.git")

	plain := filepath.Join(root, "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}

	want := "github.com/jj-scoll/broodhall"
	for name, dir := range map[string]string{
		"https clone root":     a,
		"nested dir":           filepath.Join(a, "pkg", "deep"),
		"scp clone elsewhere":  b,
		"linked git worktree":  wt,
		"worktree via dot-git": wt + "/",
	} {
		if got := projectKey(dir); got != want {
			t.Errorf("%s: projectKey = %q, want %q", name, got, want)
		}
	}
	if got := projectKey(sub); got != "github.com/jj-scoll/child" {
		t.Errorf("submodule: projectKey = %q, want its own origin", got)
	}

	// No origin (or an unusable local one): basename plus a stable hash of the root path.
	k1, k2 := projectKey(noRemote), projectKey(filepath.Join(noRemote, "."))
	if !reHashSuffix.MatchString(k1) || k1[:len("lonely-")] != "lonely-" || k1 != k2 {
		t.Errorf("no-origin repo: %q / %q", k1, k2)
	}
	if got := projectKey(noRemoteWT); got != k1 {
		t.Errorf("no-origin worktree = %q, want the main checkout's %q", got, k1)
	}
	if got := projectKey(localOrigin); !reHashSuffix.MatchString(got) {
		t.Errorf("local-path origin: %q, want basename-hash", got)
	}
	pk := projectKey(plain)
	if !reHashSuffix.MatchString(pk) || pk[:len("plain-")] != "plain-" {
		t.Errorf("non-git dir: %q", pk)
	}
	other := filepath.Join(root, "sub", "plain")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if projectKey(other) == pk {
		t.Errorf("two dirs named plain share key %q", pk)
	}
	for _, c := range []string{"", "  ", "/"} {
		if got := projectKey(c); got != memdUnknownProject {
			t.Errorf("projectKey(%q) = %q, want %q", c, got, memdUnknownProject)
		}
	}
}
