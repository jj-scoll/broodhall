// claude-tools: the guardrails plugin's Claude Code hooks as one binary, one subcommand each.
// The plugin's hooks/hooks.json runs them through scripts/run.sh.
package main

import (
	"fmt"
	"os"
)

var commands = map[string]func(args []string) int{
	"read-dedup":       readDedup,
	"large-read-guard": largeReadGuard,
	"pre-tool-use":     preToolUse,
	"redact":           redactCmd,
	"pii-rules":        piiRulesCmd,
	"retry-breaker":    retryBreaker,
	"statusline":       statusline,
	"version":          versionCmd,
}

func main() {
	if len(os.Args) < 2 || commands[os.Args[1]] == nil {
		fmt.Fprintln(os.Stderr, "usage: claude-tools <pre-tool-use|read-dedup|large-read-guard|retry-breaker|redact|pii-rules|statusline|doctor|version> [args]")
		os.Exit(2)
	}
	os.Exit(commands[os.Args[1]](os.Args[2:]))
}
