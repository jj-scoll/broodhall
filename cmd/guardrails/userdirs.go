package main

// The binary was called claude-tools before it was renamed guardrails, and a user's config
// and state stay where they were. Each file is looked up under <base>/guardrails, then
// <base>/claude-tools, and created under <base>/guardrails. Settings from the environment
// follow the same order: GUARDRAILS_<NAME>, then CLAUDE_TOOLS_<NAME>.

import (
	"os"
	"path/filepath"
)

// userPaths is every place <name> may live under base, the current name first. Guards that
// protect a file protect each of these, whichever one is in use.
func userPaths(base, name string) []string {
	return []string{filepath.Join(base, "guardrails", name), filepath.Join(base, "claude-tools", name)}
}

// userPath is where <name> lives under base: the old-name path only when it exists and the
// new one does not, so a key or rules file the user already has keeps being used.
func userPath(base, name string) string {
	ps := userPaths(base, name)
	if _, err := os.Lstat(ps[0]); err != nil {
		if _, err := os.Lstat(ps[1]); err == nil {
			return ps[1]
		}
	}
	return ps[0]
}

// settingEnv is GUARDRAILS_<name> when it is set, even empty (the launcher always exports
// it), else CLAUDE_TOOLS_<name>.
func settingEnv(name string) string {
	if v, ok := os.LookupEnv("GUARDRAILS_" + name); ok {
		return v
	}
	return os.Getenv("CLAUDE_TOOLS_" + name)
}

// stateBase is $XDG_STATE_HOME, else ~/.local/state.
func stateBase() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return dir
	}
	return filepath.Join(os.Getenv("HOME"), ".local", "state")
}
