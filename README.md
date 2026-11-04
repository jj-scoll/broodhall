# guardrails

Guards for Claude Code tool calls, as a plugin: it blocks destructive shell commands, asks
before destructive git and settings edits, masks Bash reads of secret files, blocks the
Read tool on them and asks before writes to them, blocks oversized whole-file Reads, and
notes re-reads and retry loops. The hooks are one static Go binary.

## Install

```sh
claude plugin marketplace add jj-scoll/broodhall
claude plugin install guardrails@broodhall
```

If a broken binary blocks every tool call, run `claude plugin disable guardrails@broodhall`
in a terminal and start a new session.

Memory, a separate plugin that keeps notes of your sessions in a local database, installs with:

```sh
claude plugin install memory@broodhall
```

RTK, a plugin that tells Claude how [rtk](https://github.com/jj-scoll/rtk)-condensed command
output reads, installs with:

```sh
claude plugin install rtk@broodhall
```

Documentation: https://jj-scoll.github.io/broodhall/

## License

MIT
