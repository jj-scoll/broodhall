# broodhall

broodhall is a Claude Code marketplace. Its plugin, guardrails, blocks destructive shell
commands, asks before destructive git and settings edits, masks a Bash read of a secret
file, blocks the Read tool on one, blocks oversized whole-file Reads, and notes re-reads and
retry loops. The hooks are one static Go binary.

```
claude plugin marketplace add jj-scoll/broodhall
claude plugin install guardrails@broodhall
```

Start a new session afterwards.

The marketplace also has [memory](memory/index.md), which gives Claude a searchable memory of
your past sessions: `claude plugin install memory@broodhall`.

- [Why it exists](guide/why.md): the problem, the four ways it answers, and what it is not.
- [Install](guide/install.md): platforms, what gets downloaded, and how to test a local build.
- [Guardrails overview](guardrails/index.md): every guard and its options.
