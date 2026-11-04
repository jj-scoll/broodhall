# Handoff gate

<span class="badge badge-block">BLOCK</span> the first stop of a session once its context is large, and <span class="badge badge-block">BLOCK</span> a second manual `/compact`.

!!! why "Why it exists"
    A long session loses detail every time it compacts. Writing a handoff while the context
    is still intact, and continuing from it in a fresh session, keeps more than a second or
    third compaction does. Three small hooks push toward that: one asks for a handoff before
    the session gets near auto-compact, one has it written and arms a `/clear`, and one stops
    a repeat manual compaction and points at the handoff instead. All of them only advise,
    and all can be overridden at once.

## The chain

```text
gate  ->  autoclear  ->  write-handoff  ->  context-guard runs /clear  ->  continue prompt
(Stop)    (Stop)         (background)       (separate plugin)             (fresh session)
```

1. **gate** (`Stop`). The context passes the threshold. The gate blocks the stop once and asks
   Claude for only what the transcript cannot show: it writes `.handoff-next-<session>.md` in
   the working directory, with a first line `file: <handoff file name>`, then `## Next Steps`
   and `## Open Questions`. Claude is told not to write the handoff itself.
2. **autoclear** (`Stop`). On the stop that ends that turn, it starts `claude-tools
   write-handoff <transcript> <next-steps file> <cwd> <session>` in the background and returns
   at once. It never blocks the stop.
3. **write-handoff**. Writes the handoff file from the transcript and the next-steps file,
   about a minute and a half. It prints the handoff's path on stdout and exits non-zero on
   failure. Its stderr goes to `writer.log`. When it succeeds, autoclear arms the clear by
   writing `<session>.rearm`, which holds the handoff path and the old transcript path.
4. **context-guard** ([its own plugin](../context-guard/index.md)). Once the turn has ended
   and the prompt box is empty, it takes the armed clear (`autoclear --claim`), runs `/clear`,
   and submits the continue prompt in the fresh session. A prompt you type while the clear is
   armed is dropped and carried into the fresh session after the continue prompt.
5. **Continue prompt.** `Continue from the handoff in <file>.` In a fresh session Claude reads
   the handoff. If the old transcript is still on disk the prompt adds its path, so Claude can
   search it with `rg` or `jq` for anything the handoff left out.

context-guard reports the outcome with `autoclear --done`: `ok` removes the claim, `fail`
re-arms it so the next prompt tries again.

### When a link is missing

| Missing | What happens |
| --- | --- |
| context-guard not installed | The handoff is still written and the clear stays armed, but nothing runs `/clear`. The gate's message tells Claude to name the handoff file in its last line; run `/clear` yourself and ask to continue from it. |
| `HANDOFF_AUTOCLEAR=0` | The handoff is still written, the clear is never armed and `writer.log` says so. The gate's message then tells Claude that the user runs `/clear`, instead of promising that the session clears itself. |
| The writer fails | `writer.log` gets `<session> writer failed` and the writer's own stderr. Nothing is armed; the session carries on, and auto-compact still applies. |
| The user is away | Claude keeps working. The clear waits for the prompt box to be empty, and the watch gives up after two hours. |

The older route, in which Claude ran the memory plugin's `handoff` skill itself, is gone from
the gate. That skill still works when you run it by hand, and context-guard's block message
points at it.

## The hooks

| Event | Subcommand | What it does |
| --- | --- | --- |
| `Stop` | `gate` | When the context passes the threshold, blocks the stop once and asks Claude for the next-steps file. |
| `Stop` | `autoclear` | After the gate's block, starts the handoff writer in the background and arms the clear. Also serves `--claim` and `--done` to context-guard. |
| `PreCompact` | `compact-guard` | Blocks the first manual `/compact` in a session that has already compacted. Re-arms `gate` and clears the `read-dedup` ledger. |

### gate

On every `Stop` the hook reads the context size from the transcript: the input, cache-read
and cache-creation tokens of the latest assistant message. It looks only at the last 4 MB of
the file. When the size is at or above the threshold, and this session has not been gated yet,
it blocks the stop once:

```text
Context is at 250000 tokens (handoff threshold 240000). When this turn ends, a background writer produces the handoff from this session's transcript; ... use Write to create /work/.handoff-next-<session>.md containing: a first line 'file: <handoff file name>' ... then '## Next Steps' ... and '## Open Questions'. Do not write the handoff yourself. ...
```

The threshold is the `handoff_tokens` [option](options.md#handoff_tokens). When that is empty
it is 80% of `CLAUDE_CODE_AUTO_COMPACT_WINDOW`, which is 240000 for the default window of
300000.

The gate passes, with no output, when the stop already follows a block from a `Stop` hook,
when the session id is not a plain id, when the transcript cannot be read or holds no usage
line, and on any other error. It never blocks a session twice in a row.

### autoclear

Runs on every `Stop` and does nothing unless all of these hold: the stop follows the gate's
block (`stop_hook_active` is true), the session has a gate mark, autoclear has not already run
for this handoff, the working directory exists, and `.handoff-next-<session>.md` exists and
is newer than the gate mark. It then marks the handoff as started (`<session>.autoclear`) and
launches the writer detached, so the stop is not held up. A stop that does not follow the
block does not use up the one shot.

Every outcome is a line in `writer.log` in the state directory: `writer start`, the writer's
own summary or traceback, `clear armed: <file>`, `writer failed`, `no clear:
HANDOFF_AUTOCLEAR=0`, and what context-guard reports through `--done`.

| Argument | Used by | What it does |
| --- | --- | --- |
| none (Stop input on stdin) | Claude Code | The hook above. |
| `--claim <session>` | context-guard | Moves `<session>.rearm` to `<session>.claimed`, prints the continue prompt and exits 0. Exits 1, printing nothing, when nothing is armed or the handoff file is gone. |
| `--done <session> ok\|fail <text>` | context-guard | Logs the outcome. `ok` removes the claim; `fail` puts it back as `<session>.rearm`. Exits 1 without a claim or with another outcome. |
| `--write ...` | autoclear itself | The background half that runs the writer. Not for direct use. |

### compact-guard

The hook keeps a count of compactions per session. A manual `/compact` with a count of one or
more is blocked once, with a message that points at `/memory:handoff` and `/clear`. Run
`/compact` again and it goes through. An automatic compaction is never blocked, because
blocking one that recovers from a context-limit error makes the request fail.

Every compaction that is allowed adds one to the count and removes the gate's mark, so the
gate fires again when the context next passes the threshold. It also deletes the session's
`read-dedup` ledger, so a file read before the compaction gets no "already read" note after it.

## State

The hooks keep small files named after the session id in the `claude-handoff` directory of
the plugin's data directory: `<id>.gate`, `<id>.autoclear`, `<id>.rearm`, `<id>.claimed`,
`<id>.compacts` and `<id>.compact-blocked`, plus one `writer.log` for all sessions. Set
`HANDOFF_STATE_DIR` to move the base directory (`claude-handoff` is added under it); the gate,
autoclear and context-guard must all see the same one. The hooks' launcher, `run.sh`, exports
the variable itself. When it is unset (the status line runs the binary straight from
`settings.json`, not through `run.sh`), `claude-tools` uses the plugin data directory it is
installed in, found from its own path `<data>/bin/<version>/claude-tools`, so the status line's
handoff segment reads the same files the hooks write. Outside such a layout it falls back to
`/tmp`, and an explicitly set value, even an empty one, always wins. A failure to read or write these files never
blocks anything.

### Progress markers

Four more files exist only so the [status line](statusline.md#the-handoff-segment) can show
where the chain is. Nothing else reads them.

| File | Written | Holds | Removed |
| --- | --- | --- | --- |
| `<id>.writing` | When the writer starts | Start time, epoch seconds | When the writer exits (success or failure) |
| `<id>.failed` | When the writer fails | Nothing; its age is the time of failure | When the next writer starts |
| `<id>.written` | With `HANDOFF_AUTOCLEAR=0`, once the handoff is written | The handoff path | When the next writer starts |
| `<id>.cleared` | When context-guard reports `--done ... ok` (the claim is renamed to it) | The handoff path, then the old transcript path | When the next writer starts |

A writer that is killed leaves `<id>.writing` behind; the status line ignores one older than
15 minutes.
