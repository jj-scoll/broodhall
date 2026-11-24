package main

import (
	"testing"
	"time"
)

// `command` is a wrapper unless it is a lookup (`command -v name`). A -v or -V later in the
// argv belongs to the wrapped command (docker run -v, rm -v) and must not stop the peel.
func TestCommandWrapperPeelsDespiteLaterV(t *testing.T) {
	tests := []struct {
		name, cmd, want string
	}{
		{"docker -v mount", "command docker run -v /dev:/dev ubuntu", "block:privileged-container"},
		{"rm -v after flags", "command rm -rf -v /", "block:dangerous:rm -rf /"},
		{"force push -v", "command git push --force -v origin x", "block:dangerous:git push --force"},
		{"sudo then docker -v", "command sudo docker run -v /dev:/dev ubuntu", "block:privileged-container"},
		{"command -p wrapper", "command -p docker run -v /dev:/dev ubuntu", "block:privileged-container"},
		{"lookup stays a lookup", "command -v rm", "allow"},
		{"lookup -V", "command -V docker", "allow"},
		{"lookup with -p", "command -p -v rm", "allow"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := &guardEnv{Now: time.Unix(1_800_000_000, 0), Session: "peel", Cwd: "/work", Home: "/nohome/user", State: memState{}}
			if got := decide(env, tt.cmd).Kind(); got != tt.want {
				t.Errorf("decide(%q) = %s, want %s", tt.cmd, got, tt.want)
			}
		})
	}
}
