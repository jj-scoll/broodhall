# guardrails

Guards for Claude Code tool calls. This plugin blocks destructive shell commands, asks before
destructive git and settings edits, masks Bash reads of secret files, blocks the Read tool
on them and asks before writes to them, blocks oversized whole-file Reads, and notes
re-reads and retry loops. The hooks are one static Go binary.

## Install

```sh
claude plugin marketplace add jj-scoll/broodhall
claude plugin install guardrails@broodhall
```

Once installed, the guards fail closed. If a broken binary blocks every tool call, run
`claude plugin disable guardrails@broodhall` in a terminal and start a new session.

Documentation: https://jj-scoll.github.io/broodhall/
