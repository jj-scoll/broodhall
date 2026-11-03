# Updating

```
claude plugin update guardrails@broodhall
claude plugin update memory@broodhall
```

Restart Claude Code to apply the update. At the next session start the plugin finds no
binary for the new version, downloads it, checks its sha256, and keeps it beside the old
one under `bin/<version>/` in the plugin data directory. If that download fails, the guards
are off for that session with a one-line notice, and the next session start retries.

The launcher never removes a previous version's binary, so old ones stay in `bin/` until you
delete them.

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
