# Receipt

A one-line summary, shown to you when Claude stops, of what the guards did that turn.

The guards' masks, asks, blocks and nudges mostly reach only Claude. The `Stop` hook reads
the guard ledger for the session and prints a line such as:

```text
guards · masked 2 (secret-mask) · asked 1 (egress) · nudged 4 (read-dedup 3, slow-op 1)
```

- The line is a `systemMessage`: Claude Code shows it in the transcript and does not send it
  to the model.
- In [observe mode](options.md#observe), a decision that was let through shows as `would ask`
  or `would block`, and always prints.
- It stays silent on a quiet turn. A mask, ask or block always prints; nudges print from
  three up, and fewer wait for the next receipt.
- The ledger holds only the time, kind and rule id of each event, never command text, paths or
  values.
- At Stop it also marks asks that nothing followed up as not run, for `guard-report`. The
  receipt line does not show outcomes.
- The hook fails open: it always exits 0 and makes no Stop decision, so a missing or unreadable
  ledger never blocks Stop.
