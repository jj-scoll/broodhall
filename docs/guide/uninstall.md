# Uninstall

```
claude plugin uninstall guardrails@broodhall
claude plugin uninstall memory@broodhall
```

This removes the plugin and, by default, its data directory, which holds the downloaded
binaries and the per-session guard state. Add `--keep-data` to keep that directory.

To remove the marketplace as well:

```
claude plugin marketplace remove broodhall
```

## Extra steps for memory

Uninstalling memory does not touch two things.

- **The daemon.** It is a detached background process and keeps running. Stop it with
  `pkill -f 'memory serve'`. Until you do, it keeps reading new transcripts and sending
  tool calls to Claude to write notes (see [Privacy and redaction](../memory/privacy.md)).
- **Your stored memories.** They live outside the plugin, in `~/.local/share/memory/` (or
  `$MEMORY_DIR`). Delete that directory to remove them. The optional settings file in
  `~/.config/memory/` is also yours to delete.

Stop the daemon first, then delete the directory.
