# Context guard

<span class="badge badge-note">NOTE</span> as the context grows, <span class="badge badge-block">BLOCK</span> new prompts past a limit, and run the `/clear` that ends the [handoff chain](../guardrails/handoff-gate.md#the-chain).

!!! why "Why it exists"
    The guardrails hooks can ask for a handoff and have it written, but a hook cannot run
    `/clear`. A Claude Code hooks module can: it can read the context size, empty the prompt
    box and submit a prompt in the new session. context-guard is that module.

## Install

```sh
claude plugin marketplace add jj-scoll/broodhall
claude plugin install context-guard@broodhall
```

context-guard declares guardrails as a dependency, so Claude Code installs guardrails with it.
If you already have guardrails, nothing changes. You can still install guardrails first.
Claude Code does not stop you from disabling or uninstalling guardrails later, and context-guard
stays enabled when you do. Running the context-guard install again may not bring guardrails
back (it reported "already installed" in testing with a local marketplace), so reinstall it
directly with `claude plugin install guardrails@broodhall`.

Requirements:

- **Claude Code 2.1.317 or newer.** Hooks modules are an early-access plugin kind (the
  declarations Claude Code writes say the surface may change between releases). This plugin
  was tested on 2.1.317 and no older version was tried.
- **The guardrails plugin**, which Claude Code installs for you as a dependency, at a version whose binary has the `autoclear` subcommand. Without
  it context-guard cannot find the handoff state, so it skips the clear. The warn and block
  levels still work.

## What it does

### Clear after a handoff

1. A turn ends in the main session, and the guardrails gate has marked this session
   (`<session>.gate`). The module starts a watch that checks every 3 seconds. It gives up
   after two hours.
2. When `<session>.rearm` appears, [autoclear](../guardrails/handoff-gate.md#autoclear) has
   finished writing the handoff. If the prompt box holds a draft, the watch waits for it to be
   empty.
3. The module takes the clear with `autoclear --claim`, runs `/clear`, checks that the session
   id changed, and submits `Continue from the handoff in <file>.` as a prompt in the fresh
   session. It then reports `ok` with `autoclear --done`.
4. If you submit a prompt while the clear is armed, that prompt is dropped, the clear runs at
   once, and the prompt is submitted after the continue prompt as "Then answer the prompt I
   typed just before the clear". Slash commands and `!` shell escapes are not carried: they
   go through as typed.
5. If `/clear` fails, or the session id does not change, the module reports `fail`. autoclear
   re-arms the clear, and your next prompt tries again. The watch does not retry by itself.

A subagent's turn end never starts the watch.

### Warn and block

On each prompt the module reads the context size from the session's usage.

| Context | What happens |
| --- | --- |
| below `warn_tokens` | The prompt goes through unchanged. |
| from `warn_tokens` | One line for you (`context 200k of 260k limit`) and a note for Claude to keep tool output small and delegate wide searches to subagents. |
| from `block_tokens` | The prompt is dropped, with advice to run `/memory:handoff`, then `/clear`. Slash commands, `!` shell escapes and any prompt that mentions "handoff" still go through. |

If the usage cannot be read, the prompt goes through. The warn note does not mention a
handoff, because the chain handles it: with the defaults the warn note comes at 180000, the
guardrails gate at 240000 and the block at 260000, so the handoff is normally written and the
session cleared before a prompt is ever blocked. The block is the fallback.

## Options

Set them with `/plugin configure context-guard@broodhall`.

| Option | Default | Meaning |
| --- | --- | --- |
| `warn_tokens` | `180000` | Context size from which each prompt gets the warning. |
| `block_tokens` | `260000` | Context size from which prompts are dropped. Keep it below your auto-compact window (280000 for the default 300000, which is what `CLAUDE_CODE_AUTO_COMPACT_WINDOW=300000` gives), or it never fires. |

A value that is not a positive number counts as the default. A `block_tokens` below
`warn_tokens` is raised to it, so the module never blocks before it warns.

Outside the options, the module reads no environment variables. Its launcher honours the same
two as guardrails: `CLAUDE_TOOLS_BIN` (a binary to run in place of the installed one) and
`HANDOFF_STATE_DIR` (the state directory). `HANDOFF_AUTOCLEAR=0` is read by the guardrails
hooks; see [Handoff gate](../guardrails/handoff-gate.md#when-a-link-is-missing).

## How it finds guardrails

The module runs `sh scripts/run.sh` from its own plugin directory. The launcher looks under
`$CLAUDE_CONFIG_DIR/plugins/data` (default `~/.claude/plugins/data`) for the newest
`guardrails-*/bin/*/guardrails`, which is the binary guardrails' own launcher unpacked for
its version (`guardrails-*/bin/*/claude-tools` for an older guardrails). It prints that
guardrails data directory as the state directory, because that is where the guardrails hooks
keep `claude-handoff/`. It runs only two things: `state`, and
`autoclear` with the arguments above.

If you use `CLAUDE_TOOLS_BIN` or `HANDOFF_STATE_DIR` for guardrails, set the same values in
the environment Claude Code starts in, so both plugins see them.
