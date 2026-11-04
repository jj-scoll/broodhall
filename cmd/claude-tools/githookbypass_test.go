package main

import (
	"testing"
	"time"
)

// TestGitHookBypass pins the ask for git commands that skip hooks or signing. -n means
// no-verify only for commit and am (git help commit, git help am); for push it is
// --dry-run, for merge, rebase and pull --no-stat, for revert and cherry-pick --no-commit.
func TestGitHookBypass(t *testing.T) {
	const ask = "ask:git-hook-bypass"
	tests := []struct {
		name string
		cmd  string
		want string
	}{
		// --no-verify and -n
		{"commit --no-verify", `git commit --no-verify -m x`, ask},
		{"commit -n", `git commit -n -m x`, ask},
		{"commit -nm", `git commit -nm x`, ask},
		{"commit -am then -n", `git commit -am x -n`, ask},
		{"commit message then flag", `git commit -m x --no-verify`, ask},
		{"commit amend", `git commit --amend --no-edit --no-verify`, ask},
		{"commit after -C value", `git commit -C HEAD -n`, ask},
		{"global -C", `git -C /r commit -n -m x`, ask},
		{"push --no-verify", `git push --no-verify`, ask},
		{"push --no-verify after refs", `git push origin main --no-verify`, ask},
		{"merge --no-verify", `git merge --no-verify feature`, ask},
		{"am -n", `git am -n p.mbox`, ask},
		{"am --no-verify", `git am --no-verify p.mbox`, ask},
		{"rebase --no-verify", `git rebase --no-verify main`, ask},
		{"pull --no-verify", `git pull --no-verify`, ask},
		{"inside bash -c", `bash -c 'git commit --no-verify -m x'`, ask},
		{"inside sudo", `sudo git commit -n -m x`, ask},
		{"chained", `git add -A && git commit -m x --no-verify && git push`, ask},
		{"unparseable text", `echo ${(f)x}; git commit --no-verify -m x`, ask},
		// hook path
		{"-c core.hooksPath", `git -c core.hooksPath=/dev/null commit -m x`, ask},
		{"-c core.hookspath lower case", `git -c core.hookspath=/dev/null push`, ask},
		{"--config-env hooksPath", `git --config-env core.hooksPath=HP commit -m x`, ask},
		{"config set hooksPath", `git config core.hooksPath /dev/null`, ask},
		{"config --global hooksPath", `git config --global core.hooksPath .husky`, ask},
		{"config set subcommand", `git config set core.hooksPath x`, ask},
		{"config --file hooksPath", `git config --file .git/config core.hooksPath /x`, ask},
		{"config --add hooksPath", `git config --add core.hooksPath /x`, ask},
		// environment
		{"HUSKY=0", `HUSKY=0 git commit -m x`, ask},
		{"env HUSKY=0", `env HUSKY=0 git push`, ask},
		{"HUSKY_SKIP_HOOKS=1", `HUSKY_SKIP_HOOKS=1 git commit -m x`, ask},
		{"SKIP=hook", `SKIP=eslint git commit -m x`, ask},
		{"sudo SKIP", `sudo -u dev SKIP=a,b git commit -m x`, ask},
		{"LEFTHOOK=0", `LEFTHOOK=0 git commit -m x`, ask},
		{"export HUSKY=0 first", `export HUSKY=0 && git commit -m x`, ask},
		{"HUSKY=0 on push", `HUSKY=0 git push origin main`, ask},
		// signing
		{"commit --no-gpg-sign", `git commit --no-gpg-sign -m x`, ask},
		{"-c commit.gpgsign=false", `git -c commit.gpgsign=false commit -m x`, ask},
		{"-c commit.gpgSign=off", `git -c commit.gpgSign=off commit -m x`, ask},
		{"-c tag.gpgsign=0", `git -c tag.gpgsign=0 tag v1`, ask},
		{"config commit.gpgsign false", `git config commit.gpgsign false`, ask},
		{"rebase --no-gpg-sign", `git rebase --no-gpg-sign main`, ask},
		{"cherry-pick --no-gpg-sign", `git cherry-pick --no-gpg-sign abc`, ask},
		{"tag --no-sign", `git tag --no-sign v1`, ask},

		// -n and similar flags that are not no-verify
		{"log -n", `git log -n 5`, "allow"},
		{"clean -n", `git clean -n`, "allow"},
		{"clean -fdn", `git clean -fdn`, "allow"},
		{"push -n is dry-run", `git push -n`, "allow"},
		{"push --dry-run", `git push --dry-run origin main`, "allow"},
		{"merge -n is no-stat", `git merge -n feature`, "allow"},
		{"rebase -n is no-stat", `git rebase -n main`, "allow"},
		{"pull -n is no-stat", `git pull -n`, "allow"},
		{"revert -n is no-commit", `git revert -n HEAD`, "allow"},
		{"cherry-pick -n is no-commit", `git cherry-pick -n abc`, "allow"},
		{"merge --no-verify-signatures", `git merge --no-verify-signatures x`, "allow"},
		{"commit -mn is the message n", `git commit -mn`, "allow"},
		{"after --", `git commit -m x -- -n`, "allow"},
		{"--verify wins when last", `git commit --no-verify --verify -m x`, "allow"},
		// text that only mentions a flag
		{"message mentions it", `git commit -m "use --no-verify to skip hooks"`, "allow"},
		{"message is -n", `git commit -m "-n"`, "allow"},
		{"echo", `echo "git commit --no-verify"`, "allow"},
		{"heredoc message", "git commit -F - <<'EOF'\nskip with --no-verify\nEOF", "allow"},
		// plain use
		{"commit", `git commit -m x`, "allow"},
		{"commit -S", `git commit -S -m x`, "allow"},
		{"commit --gpg-sign", `git commit --gpg-sign -m x`, "allow"},
		{"push", `git push origin main`, "allow"},
		{"config read", `git config core.hooksPath`, "allow"},
		{"config --get", `git config --get core.hooksPath`, "allow"},
		{"config --unset", `git config --unset core.hooksPath`, "allow"},
		{"config other key", `git config user.name x`, "allow"},
		{"config gpgsign true", `git config commit.gpgsign true`, "allow"},
		{"-c gpgsign true", `git -c commit.gpgsign=true commit -m x`, "allow"},
		{"-c other key", `git -c user.name=x commit -m x`, "allow"},
		{"HUSKY=1", `HUSKY=1 git commit -m x`, "allow"},
		{"empty SKIP", `SKIP= git commit -m x`, "allow"},
		{"HUSKY=0 without git", `HUSKY=0 npm ci`, "allow"},
		{"HUSKY=0 on status", `HUSKY=0 git status`, "allow"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := &guardEnv{Now: time.Unix(1_800_000_000, 0), Session: "s", Cwd: "/work", Home: "/nohome/user", State: memState{}}
			if got := decide(env, tt.cmd).Kind(); got != tt.want {
				t.Errorf("decide(%q) = %s, want %s", tt.cmd, got, tt.want)
			}
			if tt.want != "allow" {
				env.Off = guardsOff("git")
				if got := decide(env, tt.cmd).Kind(); got != "allow" {
					t.Errorf("git family off: decide(%q) = %s, want allow", tt.cmd, got)
				}
			}
		})
	}
}
