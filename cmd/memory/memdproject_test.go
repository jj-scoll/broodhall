package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile creates dir/name (and parents) with body.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestProjectKey(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "cfg"))

	// plain repo with a nested subdir
	repo := filepath.Join(root, "myrepo")
	if err := os.MkdirAll(filepath.Join(repo, "pkg", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "init", "-q")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "init")

	// real git worktree of that repo
	wt := filepath.Join(root, "feat-x")
	gitRun(t, repo, "worktree", "add", "-q", "-b", "feat-x", wt)
	if err := os.MkdirAll(filepath.Join(wt, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	// worktree whose directory name equals the parent project name -> no "parent/" doubling
	wtSame := filepath.Join(root, "other", "myrepo")
	gitRun(t, repo, "worktree", "add", "-q", "-b", "same", wtSame)

	// non-git dir
	plain := filepath.Join(root, "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}

	// non-git dir with a project marker file at an ancestor of cwd
	marked := filepath.Join(root, "marked")
	writeFile(t, filepath.Join(marked, ".memory-project"), "")
	if err := os.MkdirAll(filepath.Join(marked, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}

	// fabricated submodule: .git file pointing into <parent>/.git/modules/<name>
	subm := filepath.Join(root, "super", "libs", "child")
	writeFile(t, filepath.Join(subm, ".git"), "gitdir: ../../.git/modules/libs/child\n")

	// fabricated bare-repo worktree: gitdir <bare>/worktrees/<n> with a commondir file
	bareWT := filepath.Join(root, "barewt")
	writeFile(t, filepath.Join(bareWT, ".git"), "gitdir: "+filepath.Join(root, "store.git", "worktrees", "barewt")+"\n")
	writeFile(t, filepath.Join(root, "store.git", "worktrees", "barewt", "commondir"), "../..\n")

	// HOME stand-in
	home := filepath.Join(root, "alice")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	tests := []struct {
		name string
		cwd  string
		want string
	}{
		{"plain repo root", repo, "myrepo"},
		{"nested subdir of repo", filepath.Join(repo, "pkg", "deep"), "myrepo"},
		{"git worktree root", wt, "myrepo/feat-x"},
		{"subdir inside git worktree", filepath.Join(wt, "sub"), "myrepo/feat-x"},
		{"worktree dir named like parent", wtSame, "myrepo"},
		{"non-git dir", plain, "plain"},
		{"non-git dir with trailing slash", plain + "/", "plain"},
		{"marker file ancestor wins over cwd", filepath.Join(marked, "a", "b"), "marked"},
		{"submodule", subm, "super/libs/child"},
		{"bare-repo worktree via commondir", bareWT, "store/barewt"},
		{"HOME itself", home, "alice"},
		{"tilde home", "~", "alice"},
		{"tilde subdir", "~/proj", "proj"},
		{"empty cwd", "", "unknown-project"},
		{"blank cwd", "  \t", "unknown-project"},
		{"filesystem root", "/", "unknown-project"},
		{"nonexistent path falls back to basename", filepath.Join(root, "gone", "ghost"), "ghost"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := legacyProjectKey(tt.cwd); got != tt.want {
				t.Errorf("legacyProjectKey(%q) = %q, want %q", tt.cwd, got, tt.want)
			}
		})
	}
}

func TestProjectKeyCacheAndTimeout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cachedproj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	orig := memdGitRunner
	t.Cleanup(func() { memdGitRunner = orig })

	calls := 0
	memdGitRunner = func(string) (string, error) { calls++; return "", errors.New("not a repo") }
	for i := 0; i < 3; i++ {
		if got := projectKey(dir); !strings.HasPrefix(got, "cachedproj-") {
			t.Fatalf("projectKey = %q", got)
		}
	}
	if calls != 1 {
		t.Errorf("git ran %d times for one cwd, want 1", calls)
	}

	slow := filepath.Join(t.TempDir(), "slowproj")
	if err := os.MkdirAll(slow, 0o755); err != nil {
		t.Fatal(err)
	}
	calls = 0
	memdGitRunner = func(string) (string, error) { calls++; return "", context.DeadlineExceeded }
	projectKey(slow)
	projectKey(slow)
	if calls != 2 {
		t.Errorf("timed-out result was cached: git ran %d times, want 2", calls)
	}
	// once git answers, the real result is computed and then cached
	memdGitRunner = func(string) (string, error) { calls++; return "", errors.New("not a repo") }
	calls = 0
	projectKey(slow)
	projectKey(slow)
	if calls != 1 {
		t.Errorf("after recovery git ran %d times, want 1", calls)
	}

	// a timed-out origin lookup is not cached either
	slowOrigin := filepath.Join(t.TempDir(), "slow-origin")
	if err := os.MkdirAll(slowOrigin, 0o755); err != nil {
		t.Fatal(err)
	}
	origOrigin := memdGitOrigin
	t.Cleanup(func() { memdGitOrigin = origOrigin })
	memdGitRunner = func(string) (string, error) { return slowOrigin, nil }
	calls = 0
	memdGitOrigin = func(string) (string, error) { calls++; return "", context.DeadlineExceeded }
	projectKey(slowOrigin)
	projectKey(slowOrigin)
	if calls != 2 {
		t.Errorf("timed-out origin was cached: ran %d times, want 2", calls)
	}
	memdGitOrigin = func(string) (string, error) { return "git@github.com:o/r.git\n", nil }
	if got := projectKey(slowOrigin); got != "github.com/o/r" {
		t.Errorf("after recovery projectKey = %q", got)
	}
}
