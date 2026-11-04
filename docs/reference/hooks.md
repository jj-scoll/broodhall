# Hooks

The plugin's `hooks/hooks.json` registers these hooks. Every command runs the plugin's
`scripts/run.sh` launcher, which hands the hook's input to the `claude-tools` subcommand in
the third column. A dash means the field is absent from the entry.

| Event | Matcher | Subcommand | onFailure |
| --- | --- | --- | --- |
| `SessionStart` | — | `install` | — |
| `SessionStart` | — | `config-gate` | — |
| `SessionStart` | — | `session-env` | — |
| `ConfigChange` | <code>user_settings&#124;project_settings&#124;local_settings</code> | `config-gate` | `block` |
| `Stop` | — | `gate` | — |
| `Stop` | — | `receipt` | — |
| `PreToolUse` | <code>Bash&#124;Edit&#124;Write&#124;MultiEdit&#124;NotebookEdit&#124;mcp__.*</code> | `pre-tool-use` | `block` |
| `PreToolUse` | `Read` | `read-dedup` | `block` |
| `PreToolUse` | `Read` | `large-read-guard` | `block` |
| `PostToolUse` | `Bash` | `retry-breaker` | — |
| `PostToolUseFailure` | `Bash` | `retry-breaker` | — |
| `PreCompact` | — | `compact-guard` | — |

The entries with `onFailure: block` fail closed when the binary crashes or hangs. Those same
subcommands (`pre-tool-use`, `read-dedup`, `large-read-guard`, `config-gate`) also fail closed
when the binary is not installed: `run.sh` exits 2 with a message on stderr, unless the
[`fail_open`](../guardrails/options.md#fail_open) option is on. Every other entry exits 0
silently then, so a missing binary never blocks a stop or a compaction. The `install` entry
sets a timeout of 120 seconds and never blocks. After `install` puts the binary in place (or
finds it there) the launcher runs `claude-tools doctor -hook`, which prints only problems and
always exits 0. See [Doctor](../guardrails/doctor.md). `config-gate` at `SessionStart` only
takes a snapshot; `session-env` does nothing unless the
[`aws_session_tag`](../guardrails/options.md#aws_session_tag) option is on.

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
