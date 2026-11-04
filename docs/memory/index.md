# Memory

memory gives Claude Code a memory that lasts between sessions. A small local daemon reads
your session transcripts, turns each tool call into a short note, and stores the notes in a
DuckDB database on your machine. At the start of a session Claude gets a summary of recent
work in that project, and it can search the rest with MCP tools.

## Install

```sh
claude plugin marketplace add jj-scoll/broodhall
claude plugin install memory@broodhall
```

The first session after install downloads the program in the background, so memory starts
with the next session. See [Install](../guide/install.md) for requirements.

## Read this first: what leaves your machine

Your transcripts are stored locally, and the database is never uploaded. But the notes are
written by a model, and that model is Claude. The daemon runs your own `claude` command
(`claude -p`, with the Haiku model `claude-haiku-5-5`) once per tool call and sends it the
tool's name, input and output. So that text goes to Anthropic, through your Claude account,
and counts against your usage. Secrets are masked before it is sent. Details and limits are
on [Privacy and redaction](privacy.md).

## How it works

1. **Daemon.** `memory serve` runs in the background. The SessionStart hook starts it. A
   second copy exits at once, because the first holds the socket.
2. **Transcript tailing.** The daemon watches `projects/` under your Claude config directory
   (`~/.claude/projects`, or `$CLAUDE_CONFIG_DIR/projects`) and reads new transcript lines as
   they are written. No hook is needed on each tool call. Secrets are masked as the lines
   are read.
3. **Observations.** Each tool call is queued, and a worker asks Claude to write one
   structured note about it: a type, a title, the facts, and the files read or changed. A
   second worker writes a summary of each session. If `claude` is missing or signed out, the
   calls stay queued and nothing is lost.
4. **SessionStart context.** When a session starts, the hook prints a block of context for
   the project: recent activity, open work items and, once, any handoff. It is capped at
   about 9.5 KB.
5. **Lookup before an edit.** Before `Edit`, `Write` or `NotebookEdit`, a PreToolUse hook
   looks the file up and, if memory knows something about it, adds a short note (at most
   1,500 bytes) to the tool call. It stays silent for files under 1,500 bytes, for
   subagents, and when the lookup takes longer than 150 ms. It never blocks the call.
6. **Tools.** An MCP server named `memory` gives Claude 11 tools, listed below.
7. **UI.** The daemon serves a local web page on `127.0.0.1:9495`. See
   [UI and Quack](ui-and-quack.md).

## The 11 tools

| Tool | Use |
| --- | --- |
| `search` | Search the notes. |
| `timeline` | Notes around a given note or session, in time order. |
| `get_observations` | Full text of notes, by ID. |
| `get_tool_uses` | The recorded tool calls themselves, by ID. |
| `work_state_read` | Read the project's to-do lists and tracked items. |
| `work_state_write` | Add or change an item on a list. |
| `save_memory` | Keep a note you write by hand. |
| `session_start_context` | The context block for a project. |
| `smart_search` | Search code symbols and files in the project. |
| `smart_outline` | List the symbols in one source file. |
| `smart_unfold` | Show one function, class or method in full. |

The three `smart_` tools run inside the MCP process, stay inside the project directory, and
refuse credential files.

## Stale notes

A note about a file is wrong once the file changes. When the daemon stores a note, it also
records a fingerprint of each file the note names: the git blob hash of the file's bytes,
the value `git hash-object` prints, with the absolute path it was taken from. A relative
path is resolved against the session's working directory. A file that cannot be read, is
larger than 1 MiB, is not a regular file or is on the secret-file list gets no fingerprint.

Where notes are shown, memory compares each fingerprint with the file now and marks the
note:

- `(file changed since)`: the file's content differs.
- `(file removed since)`: the file is gone from a directory that still exists. A rename
  counts as removed.

The mark appears in the SessionStart context, in `search` and `timeline` results and in
`get_observations`, which also lists the files. In `search`, marked notes sort after
unmarked ones within the page of results. An explicit `orderBy` date order is kept as it is.

What is left unmarked:

- Notes written before this feature, which have no fingerprint. They stay unmarked.
- Notes imported from an older store. A fingerprint taken at import time would describe
  today's file, not the file the note was about.
- Paths that do not exist here because the note came from another machine: when the
  file's directory is missing too, memory cannot tell.
- Files over 1 MiB, and files beyond the first 200 that one run checks. A note that lists
  many files can use up the 200, which leaves the older notes in that run unmarked.
- A file that was replaced by a directory, grew past 1 MiB or became unreadable since the
  note was written. Memory cannot compare it, so it shows as unknown and the note stays
  unmarked.

The check is cheap on purpose. Each run (one context build or one tool call) hashes each
path at most once and at most 200 files. The SessionStart context looks at 20 notes. The
`smart_search` tool searches source code, not notes, so it has no marks.

Two limits. The fingerprint is taken when the worker stores the note, which can be a little
after the tool call, so an edit made in between is not seen. And a mark says the file
changed, not that the note is wrong.

## The handoff skill

The plugin ships a skill named `handoff`. When you stop or switch sessions, Claude writes a
`HANDOFF.md` in the project and saves a short digest with `work_state_write` in a list named
`handoff`. The next session in that project shows the digest once, at the top of the
context, with the path of the full file. A handoff older than 7 days is not shown unless you
change `handoff_max_age_hours` in [Options](options.md). A resumed session neither shows nor
uses it up.

## It fails open

Every hook exits 0, whatever goes wrong: no binary, a failed download, a bad checksum, an
unsupported platform, a crash, a hung program or a stopped daemon. A hook command that runs
longer than 3 seconds is killed and its output dropped. The session carries on without
memory. That is the opposite of [guardrails](../guardrails/index.md), which blocks when it
fails. The reason is the job: a guard that fails open protects nothing, but a memory that
fails closed would stop your work for no gain.

## Where data lives

| What | Where |
| --- | --- |
| Database, daemon log, Quack token | `~/.local/share/memory/` (see [Options](options.md)) |
| Settings file (optional) | `~/.config/memory/settings.json` |
| Daemon socket | `$XDG_RUNTIME_DIR/memory.sock` |
| Downloaded program and extensions | the plugin data directory, under `bin/<version>/` |
| Launcher log | `memory.log` in the plugin data directory |

The daemon needs `XDG_RUNTIME_DIR` for its socket. The launcher sets a private temporary
directory when your system has none, as on macOS.

## Pages

- [Privacy and redaction](privacy.md): what is sent, what is masked, what is stored.
- [Options](options.md): environment variables and the settings file.
- [UI and Quack](ui-and-quack.md): the local web page, and querying the database directly.
