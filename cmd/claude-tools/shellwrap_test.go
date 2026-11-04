package main

import (
	"strings"
	"testing"
	"time"
)

// TestExecWrapperAudit pins that a command handed to execution through a wrapper, an exec
// option of another tool, or a command string gets the same decision as the bare command.
// Each template runs once with an rm payload and once with a force-push payload; %s marks
// where the payload goes.
func TestExecWrapperAudit(t *testing.T) {
	payloads := []struct{ cmd, want string }{
		{"rm -rf /", "block:dangerous:rm -rf /"},
		{"git push --force origin x", "block:dangerous:git push --force"},
	}
	forms := []string{
		// peeled wrappers
		"nohup %s", "nohup -p %s", "timeout 5 %s", "timeout -s KILL 5 %s", "timeout --signal=KILL 5s %s",
		"timeout -k 5 10 %s", "timeout --foreground 1m %s", "nice %s", "nice -n 5 %s", "nice -n5 %s", "nice -5 %s",
		"ionice -c 3 %s", "ionice -n 7 -c 2 %s", "time %s", "time -p %s", "command %s", "command -p %s",
		"builtin %s", "exec %s", "exec -a foo %s", "setsid %s", "setsid -f %s", "stdbuf -oL %s",
		"stdbuf -o L -e0 %s", "flock /tmp/l %s", "flock -n /tmp/l %s", "flock -w 5 /tmp/l %s",
		"doas %s", "doas -u root %s", "sudo -u root %s", "env -i %s", "env -u FOO %s", "env FOO=1 %s",
		"env -C /tmp %s", "env -- %s", "xargs %s", "xargs -n1 %s", "xargs -I{} %s", "xargs -0 -P4 %s",
		"xargs -a list.txt %s", "xargs --max-args=1 %s", "echo a | xargs -r %s", "xargs -I {} -n 1 %s",
		"nohup timeout 5 nice -n 5 %s", "sudo nohup %s", "chronic %s", "unbuffer %s",
		// exec options of find
		"find . -exec %s {} \\;", "find . -execdir %s {} \\;", "find . -ok %s {} \\;",
		"find . -type f -name x -exec %s {} +", "find . -exec echo a \\; -exec %s \\;",
		// strings handed to a shell
		"sh -c '%s'", "zsh -c '%s'", "dash -c '%s'", "bash -lc '%s'", "bash -c '%s' arg0", "sh -c '%s' _ x",
		"bash --norc -c '%s'", "bash -O extglob -c '%s'", "sudo bash -c '%s'", "env bash -c '%s'",
		"env -S '%s'", "env -S '%s' ", "env -i -S \"%s\"", "su -c '%s'", "su - root -c '%s'", "su -lc '%s' root",
		"su --command='%s'", "script -c '%s' /dev/null", "script -qec '%s' /dev/null", "flock /tmp/l -c '%s'",
		"watch '%s'", "watch -n 1 '%s'", "watch -n1 -d '%s'", "watch %s", "parallel %s ::: a",
		"trap '%s' EXIT",
		// expansions
		"echo $(%s)", "echo `%s`", "x=$(%s)", "cat <(%s)", "diff <(%s) <(true)", "eval \"$(%s)\"",
		"echo \"$(%s)\"", "echo ${x:-$(%s)}", "cat <<EOF\n$(%s)\nEOF", "(%s)", "{ %s; }", "if true; then %s; fi",
		"for i in 1; do %s; done", "f() { %s; }; f", "true && %s", "true || %s", "%s &", "! %s",
		"tee >(%s) </dev/null", "bash <<< '%s'", "bash <<'EOF'\n%s\nEOF",
	}
	for _, p := range payloads {
		for _, f := range forms {
			cmd := strings.ReplaceAll(f, "%s", p.cmd)
			t.Run(cmd, func(t *testing.T) {
				env := &guardEnv{Now: time.Unix(1_800_000_000, 0), Session: "s", Cwd: "/work", Home: "/nohome/user", State: memState{}}
				if got := decide(env, cmd).Kind(); got != p.want {
					t.Errorf("decide(%q) = %s, want %s", cmd, got, p.want)
				}
			})
		}
	}
}

// TestExecWrapperNoFalsePositives pins that wrappers around harmless commands stay allowed.
func TestExecWrapperNoFalsePositives(t *testing.T) {
	cmds := []string{
		"timeout 5 ls /", "nice -n 5 make", "xargs -n1 echo rm -rf /", "find . -name x -exec grep rm {} \\;",
		"find / -name '*.go' -print", "watch -n 1 'ls /'", "su -c 'ls /' root", "env -u FOO ls", "flock /tmp/l ls",
		"parallel echo {} ::: a b", "trap 'echo bye' EXIT", "xargs rm -rf", "env -S 'ls -la'", "doas -u root ls /",
		"echo $(date)", "time ls", "command -v rm", "nohup sleep 1 &",
	}
	for _, cmd := range cmds {
		t.Run(cmd, func(t *testing.T) {
			env := &guardEnv{Now: time.Unix(1_800_000_000, 0), Session: "s", Cwd: "/work", Home: "/nohome/user", State: memState{}}
			if got := decide(env, cmd).Kind(); got != "allow" {
				t.Errorf("decide(%q) = %s, want allow", cmd, got)
			}
		})
	}
}
