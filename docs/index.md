# broodhall

broodhall is a Claude Code marketplace with four plugins.

| Plugin | What it does | Needs | After install |
| --- | --- | --- | --- |
| [guardrails](guardrails/index.md) | Blocks destructive shell commands, asks before destructive git and settings edits, masks Bash reads of secret files, blocks the Read tool on them and asks before writes to them, blocks oversized whole-file Reads, and notes re-reads and retry loops. | Linux or macOS on amd64 or arm64; `curl`, `tar`, and `sha256sum` or `shasum`. | Every guard family on. |
| [memory](memory/index.md) | Gives Claude a searchable memory of your past sessions, kept in a local database. | As guardrails, plus glibc 2.35 or newer and `xz` on Linux, and `claude` on `PATH`, signed in. | Records from the first session once the download lands. Context from the second. No `/plugin configure` options. |
| [context-guard](context-guard/index.md) | Runs the `/clear` at the end of the guardrails handoff chain, warns as the context grows, and blocks prompts past a limit. | guardrails (installed with it) and Claude Code 2.1.317 or newer. | On: warns at 180000 tokens, blocks at 260000. |
| [rtk](rtk/index.md) | Tells Claude how rtk-condensed command output reads. | rtk from the jj-scoll fork on `PATH`. | The session-start note is on. The command rewrite is off until you turn on the guardrails [`rtk` option](guardrails/options.md#rtk). |

```
claude plugin marketplace add jj-scoll/broodhall
claude plugin install guardrails@broodhall
```

Install the others the same way, by name. Start a new session afterwards.

## Quick start

1. Add the marketplace: `claude plugin marketplace add jj-scoll/broodhall`.
2. Enable the plugins: `claude plugin install guardrails@broodhall`, and any of the others.
3. In a new session, run `/guardrails:setup`. It walks you through a profile and your email
   domains, shows what it will write, runs doctor, and offers a harmless demo of a block.

See [Setup](guardrails/setup.md) for what it does, and [Roll out to a team](guide/team.md)
to give a whole team or organisation the same install.

The guardrails hooks are one static Go binary, also named `guardrails` (in older releases,
`claude-tools`). Its config and state directories are `~/.config/guardrails` and
`~/.local/state/guardrails`, and its environment variables are `GUARDRAILS_*`. Files you
already have under `~/.config/claude-tools` or `~/.local/state/claude-tools` keep being used,
and a `CLAUDE_TOOLS_*` variable still works while its `GUARDRAILS_*` name is unset (see
[Options](guardrails/options.md#older-names)).

- [Why it exists](guide/why.md): the problem, the four ways it answers, and what it is not.
- [Install](guide/install.md): requirements per plugin, platforms, what gets downloaded, and
  how to test a local build.
- [First session](guide/first-session.md): how to see each plugin work.
- [Changelog](guide/changelog.md): what changed in each release.
- [Guardrails overview](guardrails/index.md): every guard and its options.
