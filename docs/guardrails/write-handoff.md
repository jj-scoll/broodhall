# Write handoff

<span class="badge badge-note">NOTE</span> A subcommand, not a hook: `guardrails write-handoff` writes a session's handoff from its transcript, outside the session.

!!! why "Why it exists"
    The `handoff` skill of the [memory plugin](../memory/index.md) asks the session itself to
    write the note, which costs the crowded context more tokens at the worst moment. This
    command does the writing in a separate, tool-less `claude -p` run, so the session only
    has to hand over its own list of next steps. Something else starts it; it is not
    registered in `hooks.json`.

## Use

From a terminal, define the `guardrails` function first. [Run it from a terminal](index.md#run-it-from-a-terminal) shows how.

```text
guardrails write-handoff <transcript.jsonl> <next-steps file> <cwd> <session id>
```

| Argument | Meaning |
| --- | --- |
| `transcript.jsonl` | The session's transcript. |
| `next-steps file` | The session's Next Steps and Open Questions, as markdown. An optional first line `file: HANDOFF<name>.md` picks the output name. The file is deleted once the handoff is written. |
| `cwd` | The project directory. The handoff is written here. |
| `session id` | Used for the default name, `HANDOFF-<first 8 characters>.md`. |

The command prints the path of the handoff on stdout and a one-line summary (time, cost,
gaps found, whether the memory briefing was stored) on stderr. Before that summary, stderr
gets one line per `claude` call (`writer: in=… out=… $…`, then `fix: …` if a second call ran;
input tokens include cache reads and writes), the list of gaps the check found, and, after a
fix, the gaps that remain. A fix reply that is not a handoff is reported there too. It exits 0 on success and
non-zero on any failure, and on failure it writes no handoff and leaves the next-steps file
in place. A file name that is not `HANDOFF` followed by letters, digits, `.`, `_` or `-` and
ending in `.md` is replaced by the default name.

## What it does

1. Builds a digest of the transcript (typed prompts, assistant text, tool calls and the head
   of each result) and a facts block that a script extracts from it: the typed prompts word
   for word, the answer to every question asked, files changed, mutating git commands and
   background jobs still running. Secret-looking values are masked in both.
   The digest is capped at 200 KB (about 50k tokens, a few cents; the cap is a constant, not
   a setting). Over the cap, the oldest tool-result lines go first, then the oldest lines of
   any kind, and a line at the top says how many were omitted; the recent end is kept and
   the run never fails for size. The facts block is never cut, except that the background-jobs
   list shows the latest 10 jobs (identical descriptions count as one; "Wait for PR 834" and
   "Wait for PR 836" are two jobs) and says how many were left out. That count is also copied
   into the handoff's header, so a reader knows more jobs were running.
2. Sends the memory plugin's handoff skill, the digest, the facts and the next steps to one
   `claude -p` call that answers with a JSON object, one key per section: Goal, Where Things
   Stand, Next Action, Dead Ends, Decisions and Constraints, Locations, Open Questions.
3. Renders the sections to markdown and checks the result by script: every answered question
   carried, every path, commit and PR or issue number found in the input, every running
   background job mentioned.
4. Only when that check finds gaps, makes one more call that fixes just those. A reply that
   is not a handoff is ignored and the first draft stands. The fixed text is checked again
   and any remaining gaps are logged.
5. Writes `HANDOFF-<name>.md` to `cwd`, then offers a short briefing to the memory daemon.

## Login, model and cost

The calls use your own Claude Code login and the model `claude-haiku-5-5`. There is no key
file and no API mode: `ANTHROPIC_API_KEY` is removed from the environment of the `claude`
child, so a key left in your shell bills nothing. One handoff costs one Haiku call, or two
when the check finds gaps. The prompt asks for third-person or impersonal wording, since a
different session reads the note. The summary line shows the cost Claude Code reports.

Each call runs as `claude -p --safe-mode --no-session-persistence --strict-mcp-config
--tools "" --model claude-haiku-5-5 --effort high --output-format json`, in an empty
temporary directory. It has no tools, no MCP servers, no hooks and leaves no session behind.
The prompt replaces every `@` with a fullwidth `＠`, because `claude -p` would otherwise
attach any `@/path` in it as a file, and the handoff turns them back.

## Memory

When the memory plugin's daemon is running (its socket is `$XDG_RUNTIME_DIR/memory.sock`),
the command stores a briefing of at most 3000 characters in the `handoff` list: the path of
the full file, then the Goal, Next Action and Decisions and Constraints sections. The next
session in the project shows it once at its start, as described on the
[memory page](../memory/index.md). The briefing is best effort: if there is no daemon, or it
refuses the entry, the handoff file is still written and the summary line says
`not stored`.
