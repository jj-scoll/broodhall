# Updating

```
claude plugin update guardrails@broodhall
claude plugin update memory@broodhall
claude plugin update context-guard@broodhall
claude plugin update rtk@broodhall
```

Restart Claude Code to apply the update. At the next session start the plugin finds no
binary for the new version, downloads it, checks its sha256, and keeps it beside the old
one under `bin/<version>/` in the plugin data directory. If that download fails, you see a
one-line notice, and the next session start retries. Until the new binary lands, the guards
keep running on the previous version's binary (see below).

Each version unpacks to about 186 MB. For memory, the launcher removes the older versions
once the new daemon is running (see [memory](#memory) below). guardrails and context-guard
keep their previous binaries in `bin/` until you delete them.

## Between the update and the download

Claude Code runs a hook event's commands in parallel, and `/reload-plugins` switches to the
new version without a session start, so the new version can run before its binary exists.
That does not block your tool calls:

- guardrails runs the newest binary already installed for another version until the new one
  lands. The guard that found the binary missing starts the download itself, so a
  `/reload-plugins` needs no session start. Only guards fall back; other hooks stay silent until the new binary lands. Two installers at once are safe: one holds a lock
  and the other waits for it (a rare lock take-over can still download twice, which is harmless: the install is an atomic rename).
- With no binary at all (a first install), the guard starts the download and waits up to 25
  seconds for it (`GUARDRAILS_INSTALL_WAIT`). If it does not land, the call is blocked as
  before, unless `fail_open` is on, and the message says whether the install is running or
  failed. After a failed install the guards wait 5 minutes before they try again, and
  `run.sh install` retries at once.
- memory's MCP server (`run.sh mcp`) serves from the newest installed older binary, or waits
  up to 25 seconds for the install (`MEMORY_MCP_WAIT`). It does not exit while the download is
  running, because Claude Code would then skip the server for about 15 minutes. The server only
  forwards each request to the daemon, so an older server and a newer daemon work together.

## Versioning

guardrails, memory and context-guard share one version. Every release moves all three to the
same `X.Y.Z`, even when only one of them changed. The jump to 1.0.0 was not a breaking change:
it was the first release with the shared version. rtk is versioned on its own, in the
[jj-scoll/rtk](https://github.com/jj-scoll/rtk) repository. See the [Changelog](changelog.md).

## memory

memory updates the same way, with three differences.

The download (about 33 MB) runs in the background, so the new version starts from the session
after the one that found it.

**The daemon is replaced for you.** At session start the new binary asks the running daemon for
its version. If that daemon is older, the binary asks it, over its own socket, to finish what
it is doing and exit. The queue of pending observations lives in the database, so nothing is
lost. The launcher waits up to 10 seconds for the old daemon to go, then starts the new one.
It only ever talks to the daemon on your socket, so other daemons are not touched. A daemon
that is newer than the launcher (an older plugin version open in another account) is left
alone.

Daemons from releases before this one (up to 1.10.8) cannot be asked to exit. When the
launcher finds one, it leaves it running and writes one line to `memory.log` in the plugin
data directory saying so. To switch to the new version, stop the old daemon yourself and start
a new session:

```
pkill -f 'memory serve'
```

That command stops every memory daemon you have, so it is only needed once, for the update
that first brings the new launcher. The same applies if a daemon does not stop within the
wait: the log line names its pid, and `kill <pid>` stops just that one.

**Old binaries are removed.** Once the new daemon answers, the launcher deletes the `bin/<version>`
directories of memory versions older than the previous one. It keeps the current version, the
previous version (for a rollback and for MCP servers still open in older sessions), any newer
version, and any version a running process was started from. A failed cleanup is logged and
does not affect the session.

Your stored memories stay where they are, in the state directory.

## rtk

The rtk plugin comes from the fork's `develop` branch. Claude Code delivers a plugin update when
the plugin's version changes, so a change on that branch arrives once the fork bumps the
version in its `plugin.json`. An update does not touch the rtk binary; rebuild that as in
[Install the rtk binary](../rtk/index.md#install-the-rtk-binary).
