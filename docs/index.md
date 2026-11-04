# broodhall

broodhall is a Claude Code marketplace with four plugins.

| Plugin | What it does | Needs | After install |
| --- | --- | --- | --- |
| [guardrails](guardrails/index.md) | Blocks destructive shell commands, asks before destructive git and settings edits, masks a Bash read of a secret file, blocks oversized whole-file Reads, and notes re-reads and retry loops. | Linux or macOS on amd64 or arm64; `curl`, `tar`, and `sha256sum` or `shasum`. | Every guard family on. |
| [memory](memory/index.md) | Gives Claude a searchable memory of your past sessions, kept in a local database. | As guardrails, plus glibc 2.35 or newer and `xz` on Linux, and `claude` on `PATH`, signed in. | On from the second session. No `/plugin configure` options. |
| [context-guard](context-guard/index.md) | Runs the `/clear` at the end of the guardrails handoff chain, warns as the context grows, and blocks prompts past a limit. | guardrails (installed with it) and Claude Code 2.1.317 or newer. | On: warns at 180000 tokens, blocks at 260000. |
| [rtk](rtk/index.md) | Tells Claude how rtk-condensed command output reads. | rtk from the jj-scoll fork on `PATH`. | The session-start note is on. The command rewrite is off until you turn on the guardrails [`rtk` option](guardrails/options.md#rtk). |

```
claude plugin marketplace add jj-scoll/broodhall
claude plugin install guardrails@broodhall
```

Install the others the same way, by name. Start a new session afterwards.

The guardrails hooks are one static Go binary named `claude-tools`, so that name shows up in
paths and commands.

- [Why it exists](guide/why.md): the problem, the four ways it answers, and what it is not.
- [Install](guide/install.md): requirements per plugin, platforms, what gets downloaded, and
  how to test a local build.
- [First session](guide/first-session.md): how to see each plugin work.
- [Changelog](guide/changelog.md): what changed in each release.
- [Guardrails overview](guardrails/index.md): every guard and its options.
