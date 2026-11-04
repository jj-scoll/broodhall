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
