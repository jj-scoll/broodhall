# Updating

```
claude plugin update guardrails@broodhall
```

Restart Claude Code to apply the update. At the next session start the plugin finds no
binary for the new version, downloads it, checks its sha256, and keeps it beside the old
one under `bin/<version>/` in the plugin data directory. If that download fails, the guards
are off for that session with a one-line notice, and the next session start retries.

The launcher never removes a previous version's binary, so old ones stay in `bin/` until you
delete them.
