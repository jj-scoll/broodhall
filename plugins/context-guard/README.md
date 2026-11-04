# context-guard

Runs the `/clear` at the end of the [guardrails](../guardrails/README.md) handoff chain. When
the guardrails gate has asked for a handoff and the background writer has finished it, this
plugin clears the session and submits "Continue from the handoff in ..." in the fresh one. A
prompt you type while the clear is waiting is carried into the fresh session. It also warns as
the context grows and blocks new prompts past a limit.

It is a Claude Code hooks module (`hooks/context-guard.mjs`), an early-access plugin kind, so
it needs Claude Code 2.1.317 or newer. It needs the guardrails plugin, and the guardrails
version whose binary has the `autoclear` subcommand. Without guardrails it does nothing but
warn and block.

## Install

```sh
claude plugin marketplace add jj-scoll/broodhall
claude plugin install guardrails@broodhall
claude plugin install context-guard@broodhall
```

Options (`/plugin configure context-guard@broodhall`): `warn_tokens` (default 180000) and
`block_tokens` (default 260000).

Documentation: https://jj-scoll.github.io/broodhall/context-guard/
