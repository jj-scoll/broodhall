# Hooks

The plugin's `hooks/hooks.json` registers these hooks. Every command runs the plugin's
`scripts/run.sh` launcher, which hands the hook's input to the `claude-tools` subcommand in
the third column. A dash means the field is absent from the entry.

| Event | Matcher | Subcommand | onFailure |
| --- | --- | --- | --- |
| `SessionStart` | — | `install` | — |
| `PreToolUse` | <code>Bash&#124;Edit&#124;Write&#124;MultiEdit&#124;NotebookEdit&#124;mcp__.*</code> | `pre-tool-use` | `block` |
| `PreToolUse` | `Read` | `read-dedup` | `block` |
| `PreToolUse` | `Read` | `large-read-guard` | `block` |
| `PostToolUse` | `Bash` | `retry-breaker` | — |
| `PostToolUseFailure` | `Bash` | `retry-breaker` | — |

The entries with `onFailure: block` fail closed when the binary crashes or hangs. All the
guard launchers (every entry but `SessionStart`) also fail closed when the binary is not
installed: `run.sh` exits 2 with a message on stderr, unless the [`fail_open`](../guardrails/options.md#fail_open)
option is on. The `SessionStart` entry sets a timeout of 120 seconds and never blocks.

## memory

The memory plugin registers two hooks in its own `hooks/hooks.json`, and an MCP server in
`.mcp.json`. Each command runs the plugin's `scripts/run.sh` launcher with the argument in
the third column.

| Event | Matcher | Argument | Timeout |
| --- | --- | --- | --- |
| `SessionStart` | — | `session-start` | 15 s |
| `PreToolUse` | <code>Edit&#124;Write&#124;NotebookEdit</code> | `lookup` | 5 s |

`session-start` starts the daemon (`memory serve`) and prints the context block with
`memory context`. If the binary is not installed yet, it starts a background install and
prints nothing. `lookup` runs `memory lookup`. The MCP server `memory` runs
`run.sh mcp`, which hands over to `memory mcp`.

Neither hook has `onFailure: block`. The launcher always exits 0, prints nothing on any
failure, and kills the program after `MEMORY_HOOK_TIMEOUT` seconds (3 by default), so every
memory hook fails open.
