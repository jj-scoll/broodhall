package main

import (
	"testing"
	"time"
)

// TestUnparsedDestructive pins the fallback for text mvdan cannot parse (zsh-only syntax such
// as ${(f)x} and =(cmd), or a broken quote): the legacy substrings still block the exact
// spellings, and any other spelling of rm -rf on a protected target or a force push asks.
func TestUnparsedDestructive(t *testing.T) {
	const ask = "ask:unparsed-destructive"
	tests := []struct {
		name string
		cmd  string
		want string
	}{
		{"zsh flag expansion then rm -fr", `echo ${(f)x}; rm -fr /`, ask},
		{"zsh =() then rm -fr", `cat =(echo hi); rm -fr /`, ask},
		{"double space defeats the substring", `rm  -rf  /; echo 'x`, ask},
		{"rm -r -f split flags", `echo ${(f)x}; rm -r -f ~`, ask},
		{"rm -fr $HOME", `echo ${(f)x}; rm -fr $HOME`, ask},
		{"sudo rm", `echo ${(f)x}; sudo rm -fr /`, ask},
		{"force push", `echo 'x; git push  --force origin x`, ask},
		{"force push -f", `echo ${(f)x}; git push origin -f`, ask},
		{"inner script unparseable", `bash -c 'echo "a; rm -fr /'`, ask},
		{"exact substring still blocks", `echo "a; rm -rf /`, "block:dangerous:rm -rf /"},
		{"exact force push still blocks", "git push --force origin x; echo \"a", "block:dangerous:git push --force"},
		{"harmless unparseable", `echo "unterminated`, "allow"},
		{"harmless zsh syntax", `echo ${(f)x}`, "allow"},
		{"rm of a subdir", `echo ${(f)x}; rm -fr /tmp/x`, "allow"},
		{"force-with-lease keeps the legacy block", `echo ${(f)x}; git push --force-with-lease`, "block:dangerous:git push --force"},
		{"force-with-lease after other spacing", `echo ${(f)x}; git  push --force-with-lease`, "allow"},
		{"plain rm -f", `echo ${(f)x}; rm -f /`, "allow"},
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
