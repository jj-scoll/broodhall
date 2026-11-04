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

## Token cost and cache misses

The daemon also records the token counts of every Claude response in the transcripts: model,
input, output, cache writes (split into 5-minute and 1-hour when the transcript has it) and
cache reads, with the session, project and time. No text is stored. A response appears in a
transcript once per content block, so rows are keyed by `message.id` plus `requestId` and
counted once. The first start reads your existing transcripts once to fill in history.

`memory cost` prints a short report at API list prices: totals per day and per session, and
the turns with a large cache write, each tied to a likely cause.

```sh
sh "<plugin dir>/scripts/run.sh" cost --days 7
```

| Cause | Rule |
| --- | --- |
| (c) first/clear/compact | No earlier response in the thread, or the first after `/clear` or a compaction. |
| (a) idle | The gap since the previous response was longer than the cache lifetime: 5 minutes, or 1 hour when the previous write was all 1-hour. Without the 5m/1h split it assumes 5 minutes. |
| (b) model switch | The model changed. The cache is per model. |
| (d) unexplained | Warm cache, same model: new content, or something else changed the prefix. |

Rules apply in that order, except that idle ranks above a model switch, since the entry had
expired either way. Transcript times are when a block was written, not when the request
started, so gaps near 5 minutes or 1 hour can be off. Subagent and observer turns are left
out unless you pass `--include-sidechain`. Other flags: `--days N` (0 for all time),
`--min-cache N` (tokens that count as large, default 20000), `--top N`.

Prices are a dated table in `cmd/memory/memdcost.go`. Models not in it count as $0 and are
named in the report.

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
