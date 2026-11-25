package main

import (
	"runtime"
	"testing"
	"time"
)

// TestInformationalCommands pins the commands that only look destructive: a dry-run push, a
// help or version request, a shell that only parses (-n), and a find whose start path is one
// the rm rule allows. Each has a counterpart that still blocks or asks.
func TestInformationalCommands(t *testing.T) {
	const (
		allowed   = "allow"
		pushForce = "block:dangerous:git push +refspec"
		chmodRoot = "block:dangerous:chmod -R"
		rmHome    = "block:dangerous:rm -rf ~"
		findAsk   = "ask:find-delete"
		pushDel   = "ask:git-push-delete"
		destroy   = "ask:iac-destroy"
	)
	tests := []struct {
		name  string
		cmd   string
		want  string
		linux bool // relies on GNU getopt, which finds an option after the operands
	}{
		// git push --dry-run / -n
		{"dry-run +ref", "git push origin +main --dry-run", allowed, false},
		{"dry-run +ref, piped", "git push --dry-run origin +main 2>&1 | tee /tmp/push-preview.log", allowed, false},
		{"dry-run --delete", "git push --dry-run origin --delete feature-old", allowed, false},
		{"-n --delete", "git push -n origin --delete feature/old", allowed, false},
		{"porcelain dry-run :ref", "git push --porcelain --dry-run origin :refs/heads/main", allowed, false},
		{"dry-run --force", "git push --dry-run --force origin main", allowed, false},
		{"-fn", "git push -fn origin main", allowed, false},
		{"--no-dry-run last wins", "git push --dry-run --no-dry-run origin +main", pushForce, false},
		{"-n is a push option value", "git push -o -n origin +main", pushForce, false},
		{"--dry-run after --", "git push origin -- --dry-run +main", allowed, false},
		{"plain force", "git push --force origin main", "block:dangerous:git push --force", false},
		{"plain +ref", "git push origin +main", pushForce, false},
		{"plain delete", "git push origin --delete feature-old", pushDel, false},
		{"plain :ref", "git push origin :feature", pushDel, false},

		// help and version
		{"terraform destroy -help", "terraform destroy -help", allowed, false},
		{"terraform destroy --help piped", "terraform destroy --help | head -n 40", allowed, false},
		{"tofu destroy -help", "tofu destroy -auto-approve -help", allowed, false},
		{"terraform destroy -h", "terraform destroy -h", allowed, false},
		{"tofu destroy -h", "tofu destroy -h 2>&1 | head -n 20", allowed, false},
		{"terraform destroy after --", "terraform destroy -- -help", destroy, false},
		{"terragrunt destroy --help stays", "terragrunt destroy --help", destroy, false},
		{"terraform destroy", "terraform destroy", destroy, false},
		{"chmod --version first", "chmod --version -R 000 ~", allowed, false},
		{"chmod trailing --help", "chmod -R 000 ~ --help", allowed, true},
		{"chmod trailing --help, POSIXLY_CORRECT", "POSIXLY_CORRECT=1 chmod -R 000 ~ --help", chmodRoot, false},
		{"chmod --help after --", "chmod -R 000 -- ~ --help", chmodRoot, false},
		{"chmod -h is not help", "chmod -h -R 000 ~", chmodRoot, false},
		{"chmod plain", "chmod -R 000 ~", chmodRoot, false},
		{"chown --help first", "chown --help -R x ~", allowed, false},
		{"chown plain", "chown -R x ~", "block:dangerous:chown -R", false},
		{"rm --help first", "rm --help -rf ~", allowed, false},
		{"rm trailing --help stays", "rm -rf ~ --help", rmHome, false},
		{"rm plain", "rm -rf ~", rmHome, false},
		{"dd --help is not an option", "dd --help of=/dev/sda", "block:dangerous:dd if=", false},

		// bash -n
		{"bash -n -c", "bash -n -c 'chmod -R 000 ~'", allowed, false},
		{"zsh -n -c", "zsh -n -c 'chmod -R 000 ~'", allowed, false},
		{"sh -nc", "sh -nc 'rm -rf ~'", allowed, false},
		{"bash -n heredoc", "bash -n <<'EOF'\nrm -rf ~\nEOF", allowed, false},
		{"-n after the script is an argument", "bash -c 'rm -rf ~' -n", rmHome, false},
		{"bash -c", "bash -c 'chmod -R 000 ~'", chmodRoot, false},

		// find: the rm rule's paths
		{"find ./dist -delete", "find ./dist -mindepth 1 -delete", allowed, false},
		{"find prune exec rm", "find . -name __pycache__ -type d -prune -exec rm -rf {} +", allowed, false},
		{"find scratch dir -delete", "find /var/cache/app -maxdepth 1 -type f -name 'pytest-*.log' -delete", allowed, false},
		{"find cache -delete", "find ./node_modules/.cache -type f -delete", allowed, false},
		{"find no path", "find -name '*.pyc' -delete", allowed, false},
		{"find -L start", "find -L build -delete", allowed, false},
		{"find ~ -delete", "find ~ -delete", findAsk, false},
		{"find $HOME exec rm", "find $HOME -name x -exec rm {} \\;", findAsk, false},
		{"find / -delete", "find / -name '*.tmp' -delete", findAsk, false},
		{"find second start", "find ./dist ~ -delete", findAsk, false},
		{"find /home glob", "find /hom[e] -delete", findAsk, false},
		{"find exec rm root", "find ./dist -exec rm -rf / \\;", rmRootAny, false},
		{"find -print", "find ~ -name x -print", allowed, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.linux && runtime.GOOS != "linux" {
				t.Skip("BSD getopt stops at the first operand")
			}
			env := &guardEnv{Now: time.Unix(1_800_000_000, 0), Session: "s", Cwd: "/work", Home: "/nohome/user", State: memState{}}
			got := decide(env, tt.cmd).Kind()
			if tt.want == rmRootAny {
				if got == allowed {
					t.Errorf("decide(%q) = allow, want a block or ask", tt.cmd)
				}
				return
			}
			if got != tt.want {
				t.Errorf("decide(%q) = %s, want %s", tt.cmd, got, tt.want)
			}
		})
	}
}

// rmRootAny stands for any decision but allow.
const rmRootAny = "any-block"
