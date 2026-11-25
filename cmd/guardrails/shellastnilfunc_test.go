package main

import (
	"testing"
	"time"
)

// The parser accepts text such as "()0" as a FuncDecl with no name; selfPipeBomb used to
// dereference it and panic, so one such command crashed the hook (found by FuzzDecide).
func TestParseShellFuncDeclWithoutName(t *testing.T) {
	for _, cmd := range []string{"()0", "() 0", "(){ :; }", "echo hi; ()0"} {
		t.Run(cmd, func(t *testing.T) {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("decide(%q) panicked: %v", cmd, p)
				}
			}()
			env := &guardEnv{Now: time.Unix(1_800_000_000, 0), Session: "nilfunc", Cwd: "/work", Home: "/nohome/user", State: memState{}}
			_ = decide(env, cmd)
		})
	}
}
