# Updating

```
claude plugin update guardrails@broodhall
claude plugin update memory@broodhall
claude plugin update context-guard@broodhall
claude plugin update rtk@broodhall
```

Restart Claude Code to apply the update. At the next session start the plugin finds no
binary for the new version, downloads it, checks its sha256 against the `checksums.txt` that
came with the update (see [How a download is checked](install.md#how-a-download-is-checked)),
and keeps it beside the old
one under `bin/<version>/` in the plugin data directory. If that download fails, you see a
one-line notice, and the next session start retries. Until the new binary lands, the guards
keep running on the previous version's binary (see below).

The launcher never removes a previous version's binary, so old ones stay in `bin/` until you
delete them.

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

memory updates the same way, with two differences. The download (about 33 MB) runs in the
background, so the new version starts from the session after the one that found it. And
the daemon from the old version keeps running, because it holds the socket and a new daemon
exits at once when the socket is taken. To switch to the new version, stop the old one and
start a new session:

```
pkill -f 'memory serve'
```

Your stored memories stay where they are, in the state directory.

## rtk

The rtk plugin comes from the fork's `develop` branch. Claude Code delivers a plugin update when
the plugin's version changes, so a change on that branch arrives once the fork bumps the
version in its `plugin.json`. An update does not touch the rtk binary; rebuild that as in
[Install the rtk binary](../rtk/index.md#install-the-rtk-binary).
