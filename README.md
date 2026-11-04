# broodhall

A Claude Code marketplace with four plugins.

| Plugin | What it does | Needs | After install |
| --- | --- | --- | --- |
| guardrails | Blocks destructive shell commands, asks before destructive git and settings edits, masks Bash reads of secret files, blocks oversized whole-file Reads, and notes re-reads and retry loops. | Linux or macOS on amd64 or arm64; `curl`, `tar`, and `sha256sum` or `shasum`. | Every guard family on. |
| memory | Keeps notes of your sessions in a local database and gives Claude search tools over them. | As guardrails, plus glibc 2.35 or newer and `xz` on Linux, and `claude` on `PATH`, signed in. | On from the second session. |
| context-guard | Runs the `/clear` at the end of the guardrails handoff chain, warns as the context grows, and blocks prompts past a limit. | guardrails (installed with it) and Claude Code 2.1.317 or newer. | On: warns at 180000 tokens, blocks at 260000. |
| rtk | Tells Claude how [rtk](https://github.com/jj-scoll/rtk)-condensed command output reads. | rtk from the jj-scoll fork on `PATH`. | The session-start note is on. The command rewrite is off until you turn on the guardrails `rtk` option. |

The guardrails hooks are one static Go binary named `claude-tools`, so that name shows up in
paths and commands. memory has no `/plugin configure` options; it reads environment variables
and an optional settings file instead.

## Install

```sh
claude plugin marketplace add jj-scoll/broodhall
claude plugin install guardrails@broodhall
claude plugin install memory@broodhall
claude plugin install context-guard@broodhall
claude plugin install rtk@broodhall
```

Install only the ones you want. Start a new session afterwards.

If a broken binary blocks every tool call, run `claude plugin disable guardrails@broodhall`
in a terminal and start a new session.

Documentation: https://jj-scoll.github.io/broodhall/

## License

MIT
