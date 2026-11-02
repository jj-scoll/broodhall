package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Fixtures build the link at runtime so no real or literal session URL sits in the source.
// The fake ID is 24 alphanumerics ("ZZZ..."): obviously fake, same shape and length as the
// real IDs in the history (all 24 characters), so it exercises the same regex.
var (
	fakeSessionID  = "session_" + strings.Repeat("Z", 24)
	fakeSessionURL = "https://claude.ai/code/" + fakeSessionID
	trailerLine    = "Claude-Session: " + fakeSessionURL
)

func sessEnv(unpushed func(dir string) string) *guardEnv {
	return &guardEnv{Now: t0, Session: "s", Cwd: "/repo", Home: "/home/user", State: memState{}, GitUnpushed: unpushed}
}

func TestSessionURLRuleCommandText(t *testing.T) {
	heredoc := "gh pr create --title t --body \"$(cat <<'EOF'\nSummary\n\n" + fakeSessionURL + "\nEOF\n)\""
	tests := []struct {
		name  string
		cmd   string
		block bool
	}{
		{"pr create body flag", `gh pr create --title t --body "fixes it ` + fakeSessionURL + `"`, true},
		{"pr create body heredoc", heredoc, true},
		{"pr create -b short flag", `gh pr create -t t -b "x ` + fakeSessionURL + `"`, true},
		{"pr edit", `gh pr edit 12 --body "x ` + fakeSessionURL + `"`, true},
		{"trailer text", `gh pr create --body "done\n` + trailerLine + `"`, true},
		{"after cd and &&", `cd sub && gh pr create --body "` + fakeSessionURL + `"`, true},
		{"gh with repo flag", `gh -R org/repo pr create --body "` + fakeSessionURL + `"`, true},
		{"commit then push in one command", `git commit -m "feat` + "\n\n" + trailerLine + `" && git push`, true},
		{"id with trailing punctuation", `gh pr create --body "(` + fakeSessionURL + `)."`, true},

		// rule text and mentions must pass: 128 Write/Edit calls in the history carry it
		{"rule text with ellipsis", `gh pr create --body "Never write a Claude-Session: line or a claude.ai/code/session_… link"`, false},
		{"rule text with placeholder", `gh pr create --body "no claude.ai/code/session_<id> URLs, no Claude-Session: trailer"`, false},
		{"bare trailer name", `gh pr create --body "strip the Claude-Session: trailer"`, false},
		{"short fake id", `gh pr create --body "see claude.ai/code/session_abc"`, false},
		{"pr view is not an egress", `gh pr view 12 --comments # ` + fakeSessionURL, false},
		{"echo is not an egress", `echo ` + fakeSessionURL, false},
		{"grep for it is not an egress", `grep -rn "` + fakeSessionID + `" docs/`, false},
		{"commit alone is not checked", `git commit -m "x ` + fakeSessionURL + `"`, false},
		{"gh pr create clean", `gh pr create --title t --body "plain"`, false},
		{"git push clean", `git push -u origin feat`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := sessionURLRule(sessEnv(nil), tt.cmd)
			if got := d.Exit == 2; got != tt.block {
				t.Fatalf("block = %v (%s), want %v", got, d.Kind(), tt.block)
			}
			if tt.block {
				if d.Rule != "session-url" {
					t.Errorf("Rule = %q", d.Rule)
				}
				if strings.Contains(d.Msg, fakeSessionID) {
					t.Errorf("message must not echo the session ID: %q", d.Msg)
				}
			}
		})
	}
}

func TestSessionURLRuleBodyFile(t *testing.T) {
	dir := t.TempDir()
	dirty := filepath.Join(dir, "dirty.md")
	clean := filepath.Join(dir, "clean.md")
	os.WriteFile(dirty, []byte("body\n\n"+fakeSessionURL+"\n"), 0o644)
	os.WriteFile(clean, []byte("body\n\nNever write a Claude-Session: line.\n"), 0o644)
	env := sessEnv(nil)
	env.Cwd = dir
	for _, tt := range []struct {
		cmd   string
		block bool
	}{
		{"gh pr create --title t --body-file " + dirty, true},
		{"gh pr create --title t --body-file=" + dirty, true},
		{"gh pr create -F " + dirty, true},
		{"gh pr edit 3 -F dirty.md", true}, // relative to cwd
		{"gh pr create --body-file " + clean, false},
		{"gh pr create --body-file " + filepath.Join(dir, "missing.md"), false},
		{"gh pr create --body-file -", false},
	} {
		if got := sessionURLRule(env, tt.cmd).Exit == 2; got != tt.block {
			t.Errorf("%q: block = %v, want %v", tt.cmd, got, tt.block)
		}
	}
}

func TestSessionURLRuleGitPush(t *testing.T) {
	dirty := "\x01abc1234\nfeat: x\n\n" + trailerLine + "\n\x01def5678\nfix: y\n"
	clean := "\x01abc1234\nfeat: x\n\x01def5678\nfix: y\n"
	var asked []string
	spy := func(out string) func(string) string {
		return func(dir string) string { asked = append(asked, dir); return out }
	}
	t.Run("dirty unpushed commit blocks and names the hash", func(t *testing.T) {
		asked = nil
		d := sessionURLRule(sessEnv(spy(dirty)), "git push -u origin feat")
		if d.Exit != 2 || !strings.Contains(d.Msg, "abc1234") || strings.Contains(d.Msg, "def5678") {
			t.Fatalf("got %s %q", d.Kind(), d.Msg)
		}
		if strings.Contains(d.Msg, fakeSessionID) {
			t.Errorf("message must not echo the ID: %q", d.Msg)
		}
		if len(asked) != 1 || asked[0] != "/repo" {
			t.Errorf("asked %v, want [/repo]", asked)
		}
	})
	t.Run("clean commits allow", func(t *testing.T) {
		if d := sessionURLRule(sessEnv(spy(clean)), "git push"); d.Exit != 0 {
			t.Errorf("got %s", d.Kind())
		}
	})
	t.Run("rule text in an unpushed message allows", func(t *testing.T) {
		msg := "\x01abc1234\ndocs: rule\n\nNever write a Claude-Session: line or claude.ai/code/session_… link\n"
		if d := sessionURLRule(sessEnv(spy(msg)), "git push"); d.Exit != 0 {
			t.Errorf("got %s", d.Kind())
		}
	})
	t.Run("no git lookup in replay", func(t *testing.T) {
		if d := sessionURLRule(sessEnv(nil), "git push"); d.Exit != 0 {
			t.Errorf("got %s", d.Kind())
		}
	})
	t.Run("git -C and cd pick the repo", func(t *testing.T) {
		asked = nil
		sessionURLRule(sessEnv(spy(clean)), "git -C /other/repo push")
		sessionURLRule(sessEnv(spy(clean)), "cd /third && git push")
		sessionURLRule(sessEnv(spy(clean)), "git -c core.x=1 push origin x")
		if strings.Join(asked, ",") != "/other/repo,/third,/repo" {
			t.Errorf("asked %v", asked)
		}
	})
	t.Run("other git subcommands do not look", func(t *testing.T) {
		asked = nil
		for _, c := range []string{"git status", "git log --oneline", "git commit -m x", "echo git push", `git commit -m "git push later"`} {
			sessionURLRule(sessEnv(spy(dirty)), c)
		}
		if len(asked) != 0 {
			t.Errorf("looked at %v", asked)
		}
	})
}

// Real git: unpushed commit blocks, amended commit allows, a commit already on a remote
// ref is not this gate's business (history rewrite is the user's call).
func TestSessionURLGitIntegration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	remote, work := filepath.Join(root, "remote.git"), filepath.Join(root, "work")
	git(root, "init", "-q", "--bare", remote)
	git(root, "init", "-q", "-b", "main", work)
	git(work, "remote", "add", "origin", remote)
	os.WriteFile(filepath.Join(work, "a"), []byte("a"), 0o644)
	git(work, "add", "a")
	git(work, "commit", "-q", "-m", "old\n\n"+trailerLine) // already pushed below
	git(work, "push", "-q", "origin", "main")
	os.WriteFile(filepath.Join(work, "b"), []byte("b"), 0o644)
	git(work, "add", "b")
	git(work, "commit", "-q", "-m", "new\n\n"+trailerLine)

	env := sessEnv(gitUnpushed)
	env.Cwd = work
	if d := sessionURLRule(env, "git push"); d.Exit != 2 {
		t.Fatalf("unpushed commit with trailer: %s, want block", d.Kind())
	}
	git(work, "commit", "-q", "--amend", "-m", "new")
	if d := sessionURLRule(env, "git push"); d.Exit != 0 {
		t.Fatalf("after amend: %s %q, want allow (the pushed commit is not rechecked)", d.Kind(), d.Msg)
	}
	if d := sessionURLRule(env, "git -C "+work+" push"); d.Exit != 0 {
		t.Fatalf("-C form: %s", d.Kind())
	}
	// A directory that is not a repository fails open: the push itself would fail.
	env.Cwd = root
	if d := sessionURLRule(env, "git push"); d.Exit != 0 {
		t.Errorf("non-repo: %s", d.Kind())
	}
}

// End to end through the hook entry point: exit 2 + stderr, nothing on stdout.
func TestPreToolUseBlocksSessionURL(t *testing.T) {
	t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
	in := bashInput("PreToolUse", "sess-u", "/tmp", `gh pr create --body "`+fakeSessionURL+`"`, "")
	code, out, errOut := runHook(t, preToolUse, in)
	if code != 2 || out != "" || !strings.Contains(errOut, "BLOCKED") || strings.Contains(errOut, fakeSessionID) {
		t.Errorf("exit %d stdout %q stderr %q", code, out, errOut)
	}
}
