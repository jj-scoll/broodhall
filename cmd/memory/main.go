// memory: a local memory daemon for Claude Code, and the thin hook and MCP clients that talk
// to it. One binary, one subcommand each:
//
//	memory serve    the daemon: tails session transcripts, writes observations, serves the
//	                unix socket API and the loopback UI
//	memory context  SessionStart hook: prints the project's memory context
//	memory lookup   PreToolUse hook: attaches what memory knows about a file to Edit/Write
//	memory compact-capture  PostCompact hook: spools the compaction summary for the daemon
//	memory mcp      MCP stdio server that forwards tool calls to the daemon
//	memory cost     token cost at list prices and why the big cache writes happened
//	memory version  the release and commit this binary was built from
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

var commands = map[string]func(args []string) int{
	"serve":           func([]string) int { return memdServeCmd() },
	"context":         memContext,
	"lookup":          func([]string) int { return memLookup(os.Stdin, os.Stdout) },
	"compact-capture": func([]string) int { return memCompactCaptureCmd(os.Stdin, os.Stderr) },
	"mcp":             func([]string) int { return memMCP(os.Stdin, os.Stdout, memdSockPath()) },
	"cost":            func(args []string) int { return memCostTo(args, memdSockPath(), os.Stdout, os.Stderr) },
	"version":         func([]string) int { return memVersionTo(os.Stdout) },
}

func main() { os.Exit(run(os.Args[1:], os.Stderr)) }

func run(args []string, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: memory <serve|context|lookup|compact-capture|mcp|cost|version>")
		return 2
	}
	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "memory: unknown subcommand %q (want %s)\n", args[0], "serve|context|lookup|compact-capture|mcp|cost|version")
		return 2
	}
	return cmd(args[1:])
}

// memdServeCmd runs `memory serve` until SIGTERM or SIGINT.
func memdServeCmd() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := memdServe(ctx, memdDir(), defaultMemdRoots()); err != nil {
		fmt.Fprintln(os.Stderr, "memory serve:", err)
		return 1
	}
	return 0
}

// memLookup is `memory lookup`, the PreToolUse hook. It reads the hook payload from in and
// prints an additionalContext reply when memory knows something about the file the tool call
// touches. It stays silent (exit 0, no output) for every other case, including any error.
func memLookup(in io.Reader, out io.Writer) int {
	data, err := io.ReadAll(in)
	if err != nil {
		return 0
	}
	var p struct {
		Cwd       string         `json:"cwd"`
		ToolName  string         `json:"tool_name"`
		ToolInput map[string]any `json:"tool_input"`
		AgentID   string         `json:"agent_id"`
		AgentID2  string         `json:"agentId"`
	}
	if json.Unmarshal(data, &p) != nil {
		return 0
	}
	if t := memdToolLookup(p.ToolName, p.ToolInput, p.Cwd, p.AgentID != "" || p.AgentID2 != ""); t != "" {
		emitContext(out, "PreToolUse", t)
	}
	return 0
}

// emitContext prints an additionalContext reply: model-visible, shown next to the tool
// result, never blocks.
func emitContext(w io.Writer, event, msg string) {
	type hookOut struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	enc.Encode(struct {
		HookSpecificOutput hookOut `json:"hookSpecificOutput"`
	}{hookOut{event, msg}})
}
