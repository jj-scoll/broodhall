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

The first session after install downloads the program in the background. When the download
lands, the daemon starts and records the rest of that session. The context block at session
start begins with the next session. See [Install](../guide/install.md) for requirements.

## Read this first: what leaves your machine

Your transcripts are stored locally, and the database is never uploaded. But the notes are
written by a model, and that model is Claude. The daemon runs your own `claude` command
(`claude -p`, with the Haiku model `claude-haiku-5-5`) once per tool call and sends it the
tool's name, input and output. For the session summaries it also sends your prompts and the
titles of the session's notes. So that text goes to Anthropic, through your Claude account,
and counts against your usage. Secrets are masked before it is sent. Details and limits are
on [Privacy and redaction](privacy.md).

## How it works

1. **Daemon.** `memory serve` runs in the background. The SessionStart hook starts it. A
   second copy exits at once, because the first holds the socket.
2. **Transcript tailing.** The daemon watches `projects/` under every Claude config directory
   (`~/.claude`, each other `~/.claude*` directory that has a `projects/`, and
   `$CLAUDE_CONFIG_DIR`) and reads new transcript lines as they are written. So one database
   holds all your accounts, whichever account started the daemon. No hook is needed on each
   tool call. Secrets are masked as the lines are read.
3. **Observations.** Each tool call is queued, and a worker asks Claude to write one
   structured note about it: a type, a title, the facts, and the files read or changed. A
   second worker writes a short summary of the session after each burst of turns, a minute
   after the first turn of the burst ends. If `claude` is missing or signed out, the
   calls stay queued and nothing is lost.
4. **SessionStart context.** When a session starts, the hook prints a block of context for
   the project: recent activity, open work items and, once, any handoff. It is capped at
   about 9.5 KB.
5. **Lookup before an edit.** Before `Edit`, `Write` or `NotebookEdit`, a PreToolUse hook
   looks the file up and, if memory knows something about it, adds a short note (at most
   1,500 bytes) to the tool call. It stays silent for files under 1,500 bytes, for
   subagents, and when the lookup takes longer than 150 ms. It never blocks the call.
6. **Compaction summaries.** When Claude Code compacts a session (`/compact` or automatic),
   a PostCompact hook runs `memory compact-capture`. It masks secrets in the summary and
   writes it to `compactions/` in the state directory, and never opens the database. The
   daemon reads that folder every few seconds and stores each summary as a session summary
   titled "Context compaction (manual)" or "(auto)". `search` finds it, and
   `get_observations` with its `S<n>` ID (for example `S12`) returns the full text. The
   folder keeps summaries while the daemon is stopped, and the same compaction is never
   stored twice. A file that cannot be read as a summary moves to `compactions/rejected/`,
   which is never read back and is cleared after 7 days. The summary is filed under
   `CLAUDE_PROJECT_DIR` when it is set, else the hook's working directory. (A raw copy saved
   by the launcher when the program is missing or fails does not carry `CLAUDE_PROJECT_DIR`,
   so it is filed by the working directory.) The context block
   that follows a compaction leaves out that session's own summary, because Claude Code
   already put it in the conversation.
7. **Tools.** An MCP server named `memory` gives Claude 12 tools, listed below.
8. **UI.** The daemon serves a local web page on `127.0.0.1:9495`. See
   [UI and Quack](ui-and-quack.md).

## The 12 tools

| Tool | Use |
| --- | --- |
| `search` | Search the notes, and the raw session records when no note matches. |
| `timeline` | Notes around a given note or session, in time order. |
| `get_observations` | Full text of notes, by ID. |
| `get_tool_uses` | The recorded tool calls themselves, by ID. |
| `read_session` | One session's conversation in order: prompts, replies and tool calls. |
| `work_state_read` | Read the project's to-do lists and tracked items. |
| `work_state_write` | Add or change an item on a list. |
| `save_memory` | Keep a note you write by hand. |
| `session_start_context` | The context block for a project. |
| `smart_search` | Search code symbols and files in the project. |
| `smart_outline` | List the symbols in one source file. |
| `smart_unfold` | Show one function, class or method in full. |

The three `smart_` tools run inside the MCP process, stay inside the project directory, and
refuse credential files.

Two `work_state_write` lists are special. `facts` keeps lasting facts about the project, and
`user-facts` keeps facts for every project. Both appear at the start of every session. Write
one key per fact, and null to delete it. An item can also carry a `work_product` field, the
URL or path of something it produced, which the [UI](ui-and-quack.md) shows with the item.

## Raw search

Notes are written by a model, so they can lag behind, or not exist at all when `claude` is
signed out. Raw search needs no model. It searches the records the daemon stores as it reads
your transcripts: your prompts, Claude's replies, the inputs of tool calls and the file paths
of `Read`, `Edit`, `MultiEdit`, `Write` and `NotebookEdit` calls. Tool outputs are left out
because they are bulky. Everything is masked first (see [Privacy and redaction](privacy.md)).

`search` falls back to it when no note matches, and `source: "raw"` asks for it directly.
`source: "notes"` turns the fallback off. It returns sessions, most recently active first,
each with a short snippet around the match. Then `read_session` with the session ID returns
the conversation in order. Long sessions come in pages (`offset` and `limit`), and tool outputs
are cut short.

The rules follow Atuin Threads:

- Each word matches a whole word, ignoring case. `alpha` does not match `alphabet`. A name
  such as `parser.go` or `file_path` matches as a whole and also by its parts, so `parser`
  finds `parser.go`.
- Words in `"quotes"` match as a phrase: adjacent and in order, inside one message.
- Every word and phrase must occur somewhere in the session, though not in the same message.
- An empty query lists the newest sessions, each with its first prompt.

Filters: `project`, `dateStart` and `dateEnd` (sessions active in that range), `branch` (the
exact git branch) and `model` (a case-insensitive part of the model name, such as `opus`). Setting
`branch` or `model` makes the search a raw one, because notes carry neither. A session keeps
the branch and model last seen in its transcript.

The index is a set of `rx_` tables next to the note indexes. New rows are indexed as the daemon
stores them, and the first start after an upgrade indexes the records already there. Observer
sessions are never indexed or returned, and subagent replies are not stored. Only the first
3,000 words of each prompt, reply or tool input are indexed, which keeps a large `Write` from
filling the index. The file path of a tool call is indexed apart from that, so it always
stays findable. The index holds one row per word occurrence. In a test on 2.2 MB of source
text it came to about 340,000 rows and added 3.7 MB to the database file.

## Token cost and cache misses

The daemon also records the token counts of every Claude response in the transcripts: model,
input, output, cache writes (split into 5-minute and 1-hour when the transcript has it) and
cache reads, with the session, project and time. No text is stored. A response appears in a
transcript once per content block, so rows are keyed by `message.id` plus `requestId` and
counted once. The first start reads your existing transcripts once to fill in history.

`memory cost` prints a short report at API list prices: totals per day and per session, and
the turns with a large cache write, each tied to a likely cause.

```sh
m=$(ls -d ~/.claude/plugins/cache/broodhall/memory/*/ | sort -V | tail -1)
sh "${m}scripts/run.sh" cost --days 7
```

The first line picks the newest installed plugin version. The launcher finds the plugin data
directory from where the plugin is installed. With `CLAUDE_CONFIG_DIR` set, use it in place of
`~/.claude`.

| Cause | Rule |
| --- | --- |
| (c) first/clear/compact | No earlier response in the thread, or the first after `/clear` or a compaction. |
| (a) idle | The gap since the previous response was longer than the cache lifetime: 5 minutes, or 1 hour when the previous write was all 1-hour. Without the 5m/1h split it assumes 5 minutes. |
| (b) model switch | The model changed. The cache is per model. |
| (d) unexplained | Warm cache, same model: new content, or something else changed the prefix. |

Rules apply in that order, except that idle ranks above a model switch, since the entry had
expired either way. Transcript times are when a block was written, not when the request
started, so gaps near 5 minutes or 1 hour can be off. Subagent and observer turns are left
out unless you pass `--include-sidechain`. Other flags: `--days N` (default 7, 0 for all
time), `--min-cache N` (tokens that count as large, default 20000), `--top N` (default 10).

`memory cost` asks the running daemon for the report, so start a session first. With no
daemon it prints ``memory cost: ... (is `memory serve` running?)`` and exits 1.

Prices are a dated table in `cmd/memory/memdcost.go`. Models not in it count as $0 and are
named in the report.

## Resume a session

Claude Code's own `/resume` lists only the sessions of the current directory and account.
Memory reads the transcripts of every config directory, so `memory resume` finds a session
from any of your accounts and prints the command that resumes it. It never runs `claude`
itself.

```sh
m=$(ls -d ~/.claude/plugins/cache/broodhall/memory/*/ | sort -V | tail -1)
sh "${m}scripts/run.sh" resume --list parser       # sessions that mention "parser"
sh "${m}scripts/run.sh" resume 3f2a9c              # print the command for a session id or id prefix
eval "$(sh "${m}scripts/run.sh" resume --fork 3f2a9c)"   # run it, as a fork
```

The list is newest first. Each session shows its title (the first line of its first prompt, or
the session summary's request), the id, the project, the branch when memory has one, how long
ago it was last active, the number of prompts and the config directory it lives in.

The command looks like this:

```sh
cd -- /work/demo && CLAUDE_CONFIG_DIR=/home/me/.claude-work claude --resume 3f2a9c1e-...
```

- `CLAUDE_CONFIG_DIR` comes from where the transcript is stored. It is left out only when the
  session is in `~/.claude` and your shell is not pointing at another account. Otherwise it
  is written out, so the command resumes in the right account.
- Paths and ids are quoted for a POSIX shell.
- If the session's directory no longer exists, the command has no `cd`, and a note on stderr
  says so. Claude looks sessions up by project directory, so it may not find the session
  from another directory.
- `--fork` adds `--fork-session`, which resumes into a new session and leaves the original as
  it is.

A word that is not an id is a search. All its words must appear, in any case, in a prompt, in
a summary request, or in the project name or directory. When stdin is a terminal you pick a
result by number; the list goes to stderr and only the command goes to stdout, so
`eval "$(...)"` works. When stdin is not a terminal, the list is printed instead. `--limit N`
caps the list at N (default 20).

**Forks.** `claude --resume X --fork-session` writes a new transcript with a new session id.
Memory links it to the original and `memory resume --list` shows forks under the original,
which also shows how many forks it has. The Sessions page in the [UI](ui-and-quack.md) does
the same, with a "Fork of" link on the fork and links to the forks on the original.

Claude Code does not record the original in a fork made this way (checked with Claude Code
2.1.322). The fork repeats the original's lines, message ids included, so memory takes two
transcripts that start with the same message id to be one session and its fork. The one that
was opened first is the original. When a transcript carries a `forkedFrom` session id, which
Claude Code's in-session branch writes, that id is used instead.

**Subagents.** A subagent's transcript is stored under its parent session and carries the
parent's session id. `memory resume` never lists subagents. If you give it a subagent's id
(`agent-<id>` or the id alone) it resumes the parent session and says so on stderr.

Like `memory cost`, it asks the running daemon, so start a session first. With no daemon it
prints ``memory resume: ... (is `memory serve` running?)`` and exits 1. It exits 1 too when
nothing matches or an id prefix fits several sessions.

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
uses it up. After a compaction, a handoff not yet shown appears only if it was written in the
last 30 minutes, or within `handoff_max_age_hours` when that is shorter.

## It fails open

Every hook exits 0, whatever goes wrong: no binary, a failed download, a bad checksum, an
unsupported platform, a crash, a hung program or a stopped daemon. A hook command that runs
longer than 3 seconds is killed and its output dropped. The session carries on without
memory. That is the opposite of [guardrails](../guardrails/index.md), which blocks when it
fails. The reason is the job: a guard that fails open protects nothing, but a memory that
fails closed would stop your work for no gain.

## How a project is named

Every session, note, summary and work-state list is filed under a project key. The key
comes from the directory a session runs in:

| Directory | Key |
| --- | --- |
| A git repo with an `origin` remote | The remote as `host/path`: `github.com/jj-scoll/broodhall`. The scheme, credentials, port, trailing slash and `.git` are dropped, so the ssh and https forms of one repo, and every clone of it, share a key. |
| A linked git worktree | The same key as the main checkout, because worktrees share its remote. |
| A submodule | Its own `origin`, since it is its own repository. |
| No `origin` (or only a local-path one), or not a git repo | `<directory name>-<8 hex digits>`, the hash being of the project root path. Two directories both called `ui` get different keys; a clone elsewhere does too. |

Hosts are lower-cased. For `github.com`, `gitlab.com` and `bitbucket.org` the whole path is
lower-cased too, because those forges treat `Owner/Repo` and `owner/repo` as one repo. On any
other host the path keeps its case, since a self-hosted forge may tell them apart.

Earlier versions keyed a project by the directory name alone (`broodhall`, or
`parent/worktree` for a worktree). Those rows are not rewritten. Instead the table
`project_aliases(old_key, new_key)` makes a read for the new key also read rows filed under
the old one: search, timeline, `get_observations`, the SessionStart context, work state,
handoffs, the file lookup and the UI. New rows are written under the new key. The daemon
adds an alias the first time it sees a directory whose old key already has rows, when
a session starts there, when a tool is called from it, or when its transcript is read.

An old key that several repos shared (`ui`, `new`) is aliased to a repo only when every
session recorded under it ran inside that repo's directory, or when no session recorded a
directory at all (notes saved by hand, imports). When sessions from different directories
share the key the daemon cannot tell their notes apart, so it makes no alias and those old
rows stay under the old key, visible by that name in the UI and in `project=` filters. Where
no session recorded a directory, every repo that claims the key sees all of its rows. To
force an alias, insert it yourself:
`INSERT INTO project_aliases (old_key, new_key) VALUES ('ui', 'github.com/you/ui')`.
A repo that moved directories since its sessions were recorded is treated like a shared key.
The project list in the UI shows raw keys, so an old key and its new key appear separately.

To undo, stop the daemon and run `DROP TABLE project_aliases` with a DuckDB client on
`memory.duckdb` (Quack is read-only and refuses it). Reads then match only the key they are given; no data was
changed, so nothing else needs restoring. A daemon of this version recreates the empty table
at start and rebuilds aliases as directories are seen again, so to stay undone run the
earlier release. Notes written under the new keys stay in the database but are invisible to
that release.

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
