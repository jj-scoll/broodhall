# Hooks

The plugin's `hooks/hooks.json` registers these hooks. Every command runs the plugin's
`scripts/run.sh` launcher, which hands the hook's input to the `guardrails` subcommand in
the third column. A dash means the field is absent from the entry.

| Event | Matcher | Subcommand | onFailure |
| --- | --- | --- | --- |
| `SessionStart` | — | `install` | — |
| `SessionStart` | — | `config-gate` | — |
| `SessionStart` | — | `session-env` | — |
| `ConfigChange` | <code>user_settings&#124;project_settings&#124;local_settings</code> | `config-gate` | `block` |
| `Stop` | — | `gate` | — |
| `Stop` | — | `autoclear` | — |
| `Stop` | — | `receipt` | — |
| `PreToolUse` | <code>Bash&#124;Edit&#124;Write&#124;MultiEdit&#124;NotebookEdit&#124;mcp__.*</code> | `pre-tool-use` | `block` |
| `PreToolUse` | `Read` | `read-dedup` | `block` |
| `PreToolUse` | `Read` | `large-read-guard` | `block` |
| `PostToolUse` | `Bash` | `retry-breaker` | — |
| `PostToolUse` | <code>Write&#124;Edit&#124;MultiEdit&#124;NotebookEdit</code> | `record-edit` | — |
| `PostToolUse` | <code>Bash&#124;Edit&#124;Write&#124;MultiEdit&#124;NotebookEdit&#124;mcp__.*</code> | `ask-outcome` | — |
| `PostToolUseFailure` | `Bash` | `retry-breaker` | — |
| `PostToolUseFailure` | <code>Bash&#124;Edit&#124;Write&#124;MultiEdit&#124;NotebookEdit&#124;mcp__.*</code> | `ask-outcome` | — |
| `PreCompact` | — | `compact-guard` | — |

The entries with `onFailure: block` fail closed when the binary crashes or hangs. Those same
subcommands (`pre-tool-use`, `read-dedup`, `large-read-guard`, `config-gate`) also fail closed
when the binary is not installed: `run.sh` exits 2 with a message on stderr, unless the
[`fail_open`](../guardrails/options.md#fail_open) option is on. Every other entry exits 0
silently then, so a missing binary never blocks a stop or a compaction. When only this
version's binary is missing (after an update), the four guards (not the other hooks, which still exit 0 silently) run the newest installed older binary
instead and start the install; see [Updating](../guide/updating.md#between-the-update-and-the-download). The `install` entry
sets a timeout of 120 seconds and never blocks. After `install` puts the binary in place (or
finds it there) the launcher runs `guardrails doctor -hook`, which prints only problems and
always exits 0. See [Doctor](../guardrails/doctor.md). `config-gate` at `SessionStart` only
takes a snapshot; `session-env` does nothing unless the
[`aws_session_tag`](../guardrails/options.md#aws_session_tag) option is on.

## context-guard

The context-guard plugin registers no command hooks. Its `hooks/hooks.json` lists a hooks
module instead:

```json
{ "modules": ["./context-guard.mjs"] }
```

The module hooks two events and calls `sh "$plugin_root/scripts/run.sh"` through
`$.process.run`.

| Event | What the module does |
| --- | --- |
| `turn.complete` | After a main-session turn, starts the watch for the clear that guardrails' `autoclear` arms. |
| `prompt.submit` | Carries a prompt over a clear, adds the warning note, or drops the prompt past the block level. |

The launcher accepts `state` and `autoclear <args>`, and exits 2 for any other argument. It
prints nothing and exits 1 when it finds no state directory, or, for `autoclear`, no
guardrails binary. With `HANDOFF_STATE_DIR` set, `state` works with no binary at all. It
needs Claude Code 2.1.317 or newer. See
[Context guard](../context-guard/index.md).

## memory

The memory plugin registers three hooks in its own `hooks/hooks.json`, and an MCP server in
`.mcp.json`. Each command runs the plugin's `scripts/run.sh` launcher with the argument in
the third column.

| Event | Matcher | Argument | Timeout |
| --- | --- | --- | --- |
| `SessionStart` | — | `session-start` | 15 s |
| `PostCompact` | — | `compact-capture` | 5 s |
| `PreToolUse` | <code>Edit&#124;Write&#124;NotebookEdit</code> | `lookup` | 5 s |

`session-start` starts the daemon (`memory serve`) and prints the context block with
`memory context`. If the binary is not installed yet, it starts a background install and
prints nothing. `compact-capture` runs `memory compact-capture`, which saves the compaction
summary. If the program is missing or fails, the launcher saves the raw payload to
`compactions/` in the state directory for the daemon to pick up (see
[Privacy](../memory/privacy.md#what-is-sent)). `lookup` runs `memory lookup`. The MCP server
`memory` runs
`run.sh mcp`, which hands over to `memory mcp`. With no binary for this version, `run.sh mcp`
starts the install and serves from the newest older binary, or waits up to 25 seconds for it,
so Claude Code does not cache a failed connection.

None of the hooks has `onFailure: block`. The launcher always exits 0, prints nothing on any
failure, and kills the program after `MEMORY_HOOK_TIMEOUT` seconds (3 by default), so every
memory hook fails open.
