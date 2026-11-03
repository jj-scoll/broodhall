# Options

memory has no plugin options to configure. It reads a few environment variables and one
optional settings file. Set environment variables in the environment Claude Code runs in,
and restart Claude Code and the daemon (see below) for them to apply.

## The settings file

`~/.config/memory/settings.json`, or `$XDG_CONFIG_HOME/memory/settings.json`, or the path in
`MEMORY_SETTINGS`. The file is optional. A missing or unreadable file means all defaults. The
daemon reads the skip lists when it starts; the handoff age is read each time a session
starts.

```json
{
  "skip_tools": "skill,todowrite,askuserquestion",
  "skip_bash_patterns": "^(ls|pwd)( |$)",
  "handoff_max_age_hours": 72
}
```

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `skip_tools` | string, comma-separated | `listmcpresourcestool,slashcommand,skill,todowrite,askuserquestion` | Tool names that are never turned into notes and never sent to the model. Names match without regard to case. Your list replaces the default, it does not add to it. |
| `skip_bash_patterns` | string, one regular expression | empty | Bash commands that match are skipped. A pattern that does not compile is ignored, with a line in the daemon log. |
| `handoff_max_age_hours` | number above 0 | `168` (7 days) | How old a handoff can be and still show at session start. |

## Environment variables

| Variable | Default | Meaning |
| --- | --- | --- |
| `MEMORY_DIR` | `$XDG_DATA_HOME/memory`, else `~/.local/share/memory` | State directory: the database, the daemon log and the Quack token. |
| `MEMORY_EXT_DIR` | `<state directory>/ext` | Where the daemon finds the DuckDB extensions `fts`, `httpfs` and `quack`. The launcher sets this to the extensions that came with the download, so you rarely set it. |
| `MEMORY_SETTINGS` | `~/.config/memory/settings.json` | Path of the settings file. |
| `MEMORY_LOOKUP_SHELL` | unset | Set to `1` to run the lookup before `Bash`, `Grep` and `Glob` too. It then needs the hook matcher to include them; the plugin's matcher is <code>Edit&#124;Write&#124;NotebookEdit</code>, so on its own this has no effect. |
| `CLAUDE_CONFIG_DIR` | `~/.claude` | The daemon reads transcripts from `projects/` under it. |
| `CLAUDE_PROJECT_DIR` | the hook's working directory | Which project a hook or tool call belongs to. Claude Code sets it. |
| `XDG_RUNTIME_DIR` | set by your system | Holds `memory.sock`. The launcher falls back to a private directory under `$TMPDIR` when it is unset. |
| `XDG_DATA_HOME`, `XDG_CONFIG_HOME` | the XDG defaults | Used for the two default paths above. A relative value is ignored. |

The launcher script has four more variables for testing a build: `MEMORY_BIN` (use this
program and skip the download), `MEMORY_RELEASE_URL` (download from here), `MEMORY_HOOK_TIMEOUT`
(seconds a hook may run, default 3) and `MEMORY_NO_DAEMON=1` (do not start the daemon).

## Ports

The UI is on `127.0.0.1:9495` and Quack on `127.0.0.1:9494`. Both are fixed. If a port is
taken, the daemon logs it and carries on without that service. See
[UI and Quack](ui-and-quack.md).

## Changing a setting

The daemon starts at the first session and keeps running. A changed skip list or environment
variable applies after you stop it and start a new session:

```sh
pkill -f 'memory serve'
```

The daemon shuts down cleanly on that signal, and the next SessionStart starts a new one.

## One account at a time

There is one socket and one database file, so run only one daemon per state directory. A
second daemon on the same machine exits at once if it finds the socket held.
