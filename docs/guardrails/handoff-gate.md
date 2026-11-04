# Handoff gate

<span class="badge badge-block">BLOCK</span> the first stop of a session once its context is large, and <span class="badge badge-block">BLOCK</span> a second manual `/compact`.

!!! why "Why it exists"
    A long session loses detail every time it compacts. Writing a handoff while the context
    is still intact, and continuing from it in a fresh session, keeps more than a second or
    third compaction does. Two small hooks push toward that: one asks for a handoff before
    the session gets near auto-compact, the other stops a repeat manual compaction and points
    at the handoff instead. Both only advise, and both can be overridden at once.

## The two hooks

| Event | Subcommand | What it does |
| --- | --- | --- |
| `Stop` | `gate` | When the context passes the threshold, blocks the stop once and asks Claude to run the `memory:handoff` skill first. |
| `PreCompact` | `compact-guard` | Blocks the first manual `/compact` in a session that has already compacted. Re-arms `gate`. |

The handoff itself is written by the `handoff` skill of the [memory plugin](../memory/index.md),
which stores it for the next session in the project to show once at its start. Without the
memory plugin the gate's message names a skill that does not exist, so install both or turn
the gate off by leaving `handoff_tokens` very high.

### gate

On every `Stop` the hook reads the context size from the transcript: the input, cache-read
and cache-creation tokens of the latest assistant message. It looks only at the last 4 MB of
the file. When the size is at or above the threshold, and this session has not been gated yet,
it blocks the stop once:

```text
Context is at 250000 tokens (handoff threshold 240000). Before ending this turn, run the memory:handoff skill to write a handoff for this work, then end the turn with one line saying the handoff is written. ...
```

The threshold is the `handoff_tokens` [option](options.md#handoff_tokens). When that is empty
it is 80% of `CLAUDE_CODE_AUTO_COMPACT_WINDOW`, which is 240000 for the default window of
300000.

The gate passes, with no output, when the stop already follows a block from a `Stop` hook,
when the session id is not a plain id, when the transcript cannot be read or holds no usage
line, and on any other error. It never blocks a session twice in a row.

### compact-guard

The hook keeps a count of compactions per session. A manual `/compact` with a count of one or
more is blocked once, with a message that points at `/memory:handoff` and `/clear`. Run
`/compact` again and it goes through. An automatic compaction is never blocked, because
blocking one that recovers from a context-limit error makes the request fail.

Every compaction that is allowed adds one to the count and removes the gate's mark, so the
gate fires again when the context next passes the threshold.

## State

Both hooks keep small files named after the session id in the `claude-handoff` directory of
the plugin's data directory: `<id>.gate`, `<id>.compacts` and `<id>.compact-blocked`. A
failure to read or write them never blocks anything.
