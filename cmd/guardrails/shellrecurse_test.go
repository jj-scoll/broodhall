package main

import (
	"testing"
	"time"
)

// TestInnerShellRecursion pins that a command handed to an inner shell or run in a container
// gets the same decision as the bare command, with bind-mounted paths mapped back to the host.
func TestInnerShellRecursion(t *testing.T) {
	const (
		rmRoot  = "block:dangerous:rm -rf /"
		rmHome  = "block:dangerous:rm -rf ~"
		chown   = "block:dangerous:chown -R"
		chmod   = "block:dangerous:chmod -R 777 /"
		reset   = "ask:git-reset-hard"
		allowed = "allow"
	)
	tests := []struct {
		name string
		cmd  string
		want string
	}{
		// inner shells
		{"bash -c", `bash -c 'rm -rf ~'`, rmHome},
		{"clustered -lc", `bash -lc 'rm -rf /'`, rmRoot},
		{"clustered -euxc", `sh -euxc 'rm -rf /'`, rmRoot},
		{"abs path zsh", `/usr/bin/zsh -c 'rm -rf /'`, rmRoot},
		{"option before -c", `bash -o pipefail -c 'rm -rf /'`, rmRoot},
		{"sh in sh in sh", `sh -c "sh -c 'sh -c \"rm -rf /\"'"`, rmRoot},
		{"echo in -c is data", `bash -c 'echo rm -rf /'`, allowed},

		// container: command after the image
		{"chown via -c", `docker run -v /:/host img sh -c 'chown -R x /host'`, chown},
		{"chown of a container's own path", `docker run --rm img chown -R 1000 /data`, allowed},
		{"chown of a mounted scratch dir", `docker run --rm -v /tmp/s/rt:/opt/te img sh -c 'chown -R 1000:100 /opt/te'`, allowed},
		{"chown of a mounted scratch subdir", `docker run -v /:/host img chown -R 1000 /host/tmp/s`, allowed},
		{"chown through a home mount", `docker run -v /nohome/user:/h img chown -R 1000 /h`, chown},
		{"chown of the nix mount", `docker run -v /nix:/n img chown -R 1000 /n/store`, allowed},
		{"chown of the nix root", `docker run -v /nix:/n img chown -R 1000 /n`, chown},
		{"bare chown of root", `chown -R x /`, chown},
		{"bare chown of ~", `chown -R x ~`, chown},
		{"bare chown of /nix", `chown -R x /nix`, chown},
		{"bare chown of a scratch dir", `chown -R x /tmp/s`, allowed},
		{"exec shell", `docker exec -u root ctr bash -lc 'rm -rf $HOME'`, "block:dangerous:rm -rf $HOME"},
		{"global option value", `docker -H tcp://h:2375 run -v /nix:/n img sh -c 'chmod -R 777 /n'`, chmod},
		{"podman container run", `podman container run -it -v /:/h img rm -rf /h`, rmRoot},
		{"git inside container", `docker run img git push --force origin main`, "block:dangerous:git push --force"},
		{"ask inside container", `docker run img sh -c 'git reset --hard'`, reset},
		{"option values skipped", `docker run -w /x -e A=b --name n -v /:/host alpine rm -rf /host`, rmRoot},
		{"boolean flags", `docker run -it --rm --init -v /:/host alpine rm -rf /host`, rmRoot},
		{"value in cluster", `docker run -itv /:/host alpine rm -rf /host`, rmRoot},
		{"attached -v value", `docker run -v/:/host alpine rm -rf /host`, rmRoot},
		{"entrypoint is the command", `docker run --entrypoint rm -v /:/host alpine -rf /host`, rmRoot},
		{"double dash before image", `docker run -v /:/host -- alpine rm -rf /host`, rmRoot},
		{"nested container shell", `docker run -v /:/host img sh -c "bash -c 'rm -rf /host'"`, rmRoot},

		// bind mounts
		{"--volume", `docker run --volume /:/host alpine rm -rf /host`, rmRoot},
		{"--volume=", `docker run --volume=/:/host alpine rm -rf /host`, rmRoot},
		{"glob under mount", `docker run -v /:/host alpine sh -c "rm -rf /host/*"`, rmRoot},
		{"trailing slash", `docker run -v /:/host alpine rm -rf /host/`, rmRoot},
		{"volume options", `podman run -v /nohome/user:/data:z,ro img rm -rf /data`, rmHome},
		{"mount type=bind", `docker run --mount type=bind,source=/,target=/mnt alpine rm -rf /mnt`, rmRoot},
		{"mount src dst", `docker run --mount type=bind,src=/home,dst=/h alpine rm -rf /h`, "block:dangerous:rm -rf /home"},
		{"mount destination", `docker run --mount=source=/nix,destination=/n,type=bind alpine rm -rf /n`, "block:dangerous:rm -rf /nix"},
		{"chmod through mount", `docker run -v /nix:/n img chmod -R 777 /n`, chmod},
		{"longest target wins", `docker run -v /:/host -v /home:/host/h alpine rm -rf /host/h`, "block:dangerous:rm -rf /home"},
		{"redirect through mount", `docker run -v /:/host img sh -c 'echo x > /host/etc/passwd'`, "block:dangerous:> /etc/"},

		// a path above a mount target reaches the host through it
		{"rm of container root with a host mount", `docker run -v /:/host img sh -c 'rm -rf /*'`, rmRoot},
		{"rm above a mount target", `docker run -v /home/user:/home/user img rm -rf /home`, "block:dangerous:rm -rf /home"},

		// no false positives: a container's own paths are not the host's
		{"container own /etc", `docker run --rm ubuntu bash -c 'echo deb > /etc/apt/sources.list'`, allowed},
		{"container own root", `docker run --rm img rm -rf /`, allowed},
		{"container own /home", `docker run --rm img rm -rf /home`, allowed},
		{"container own chmod root", `docker run --rm img chmod -R 777 /`, allowed},
		{"container /etc beside a mount", `docker run -v /tmp/x:/work img sh -c 'echo a > /etc/b'`, allowed},
		{"read-only use of mount", `docker run -v /:/host img ls /host`, allowed},
		{"subpath of mount", `docker run -v /:/host img rm -rf /host/tmp/x`, allowed},
		{"mount of a scratch dir", `docker run -v /tmp/x:/work img rm -rf /work`, allowed},
		{"other mount not the target", `docker run -v /data:/host img rm -rf /host`, allowed},
		{"named volume", `docker run -v vol:/data img rm -rf /data`, allowed},
		{"mount type volume", `docker run --mount type=volume,source=/,target=/mnt img rm -rf /mnt`, allowed},
		{"quoted data", `docker run img sh -c 'echo rm -rf /'`, allowed},
		{"no command", `docker run -v /:/host img`, allowed},
		{"docker ps", `docker ps -a`, allowed},
		{"exec without command", `docker exec`, allowed},
		{"mount of home subdir", `docker run -v /home:/h img rm -rf /h/user/x`, allowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := &guardEnv{Now: time.Unix(1_800_000_000, 0), Session: "s", Cwd: "/work", Home: "/nohome/user", State: memState{}}
			if got := decide(env, tt.cmd).Kind(); got != tt.want {
				t.Errorf("decide(%q) = %s, want %s", tt.cmd, got, tt.want)
			}
		})
	}
}

// TestInnerShellStatefulRulesStayOuter pins the decision to recurse only the pure block/ask
// rules: a plan run inside a container is not a plan of the host directory.
func TestInnerShellStatefulRulesStayOuter(t *testing.T) {
	st := memState{}
	env := &guardEnv{Now: time.Unix(1_800_000_000, 0), Session: "s", Cwd: "/infra", Home: "/nohome/user", State: st}
	decide(env, `docker run img terraform plan`)
	if len(st) != 0 {
		t.Errorf("a plan inside a container marked state: %v", st)
	}
}

// TestResolvedCommandNames pins that a command name the shell computes gets the same decision
// as the plain name when the guard can work it out (an assignment earlier in the line, ANSI-C
// quoting, a static $(printf ...), brace expansion, a function, $IFS), and asks when it cannot
// but the arguments would be destructive under some name.
func TestResolvedCommandNames(t *testing.T) {
	const (
		rmRoot  = "block:dangerous:rm -rf /"
		rmHome  = "block:dangerous:rm -rf ~"
		rmHOME  = "block:dangerous:rm -rf $HOME"
		dynamic = "ask:dynamic-command"
		allowed = "allow"
	)
	tests := []struct {
		name string
		cmd  string
		want string
	}{
		{"assigned name", `c=rm; "$c" -rf ~`, rmHome},
		{"assigned git", `c=git; "$c" clean -fdx`, "ask:git-clean-f"},
		{"ansi-c name", `$'\x72\x6d' -rf ~`, rmHome},
		{"ansi-c octal", `$'\162\155' -rf /`, rmRoot},
		{"ansi-c behind nohup", `nohup $'\x67\x69\x74' stash clear`, "ask:git-stash"},
		{"ansi-c in a function", `cleanup() { $'\x67\x69\x74' stash clear; }; cleanup`, "ask:git-stash"},
		{"printf name", `"$(printf '\162\155')" -rf --no-preserve-root /`, rmRoot},
		{"backquoted printf", "`printf rm` -rf ~", rmHome},
		{"echo name", `$(echo rm) -rf ~`, rmHome},
		{"brace expansion", `{rm,-rf,~}`, rmHome},
		{"brace expansion HOME", `{rm,-rf,$HOME}`, rmHOME},
		{"shadowing function", `r() { rm "$@"; }; r -rf $HOME`, rmHOME},
		{"function with positional", `del() { rm -rf "$1"; }; del ~`, rmHome},
		{"function keyword", `function r { git reset --hard "$@"; }; r HEAD~3`, "ask:git-reset-hard"},
		{"IFS as space", `rm${IFS}-rf${IFS}$HOME`, rmHOME},
		{"unquoted value splits", `c="rm -rf"; $c ~`, rmHome},
		{"unknown name, rm args", `"$c" -rf ~`, dynamic},
		{"unknown name, git args", `$GIT reset --hard`, dynamic},
		{"unknown subst, push args", `$(which git) push --force origin main`, dynamic},

		{"editor variable", `"$EDITOR" notes.md`, allowed},
		{"python variable", `$PYTHON x.py`, allowed},
		{"default value", `${GO:-go} build ./...`, allowed},
		{"assigned ls", `c=ls; "$c" -la`, allowed},
		{"ansi-c ls", `$'\x6c\x73' -la`, allowed},
		{"brace in args", `cp main.go{,.bak}`, allowed},
		{"brace in echo", `echo {a,b}`, allowed},
		{"function body alone", `r() { rm "$@"; }`, allowed},
		{"function on a safe path", `r() { rm "$@"; }; r -rf /tmp/build`, allowed},
		{"quoted IFS", `echo "$IFS"`, allowed},
		{"run wrapper", `run() { "$@"; }; run ls -la`, allowed},
		{"run wrapper on rm", `run() { "$@"; }; run rm -rf ~`, rmHome},

		// code piped or handed to a shell: a fixed payload gets the normal rules, a computed one asks
		{"printf into sh", `printf '%s\n' 'rm -rf ~' | sh`, rmHome},
		{"echo into bash", `echo 'git reset --hard' | bash`, "ask:git-reset-hard"},
		{"base64 into sh", `echo cm0gLXJmIH4K | base64 -d | sh`, rmHome},
		{"source of printf", `source <(printf '%s\n' 'rm -rf $HOME')`, rmHOME},
		{"dot of echo", `. <(echo rm -rf /)`, rmRoot},
		{"quoted heredoc with continuation", "sh <<'EOF'\nrm \\\n  -rf \\\n  ~\nEOF", rmHome},
		{"heredoc with continuation", "bash <<EOF\nrm \\\n  -rf ~\nEOF", rmHome},
		{"computed payload into sh", `printf '%s\n' "$CMD" | sh`, "ask:opaque-shell"},
		{"generated script into bash", `./gen-script.py | bash -s`, "ask:opaque-shell"},
		{"computed source", `source <(./gen-env)`, "ask:opaque-shell"},
		{"variable into sudo sh", `echo "$x" | sudo sh`, "ask:opaque-shell"},
		{"fixed safe payload", `printf 'ls -la\n' | sh`, allowed},
		{"local script piped", `cat script.sh | bash`, allowed},
		{"computed data, not a shell", `./gen | sh -c 'wc -l'`, allowed},
		{"computed data into jq", `./gen | jq .`, allowed},
		{"base64 to a file", `echo aGkK | base64 -d > out.txt`, allowed},

		// shell commands an interpreter one-liner runs
		{"python os.system", `python3 -c 'import os; os.system("rm -rf ~")'`, rmHome},
		{"python subprocess shell", `python3 -c "import subprocess; subprocess.run('git reset --hard', shell=True)"`, "ask:git-reset-hard"},
		{"python subprocess list", `python3 -c "import subprocess; subprocess.check_call(['rm', '-rf', '/'])"`, rmRoot},
		{"perl system sh -c ARGV", `perl -e 'system("sh", "-c", $ARGV[0])' 'rm -rf $HOME'`, rmHOME},
		{"perl system string", `perl -e 'system("git clean -fdx")'`, "ask:git-clean-f"},
		{"python print", `python3 -c 'print("rm -rf ~")'`, allowed},
		{"python safe system", `python3 -c 'import os; os.system("ls -la")'`, allowed},
		{"perl print", `perl -e 'print "rm -rf ~\n"'`, allowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := &guardEnv{Now: time.Unix(1_800_000_000, 0), Session: "s", Cwd: "/work", Home: "/nohome/user", State: memState{}}
			if got := decide(env, tt.cmd).Kind(); got != tt.want {
				t.Errorf("decide(%q) = %s, want %s", tt.cmd, got, tt.want)
			}
		})
	}
}
