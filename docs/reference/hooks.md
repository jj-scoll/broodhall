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

The entries with `onFailure: block` are the ones that fail closed. The `SessionStart` entry
sets a timeout of 120 seconds.
