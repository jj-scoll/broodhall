# Uninstall

```
claude plugin uninstall guardrails@broodhall
claude plugin uninstall memory@broodhall
claude plugin uninstall context-guard@broodhall
claude plugin uninstall rtk@broodhall
```

This removes the plugin and, by default, its data directory, which holds the downloaded
binaries and the per-session guard state. Add `--keep-data` to keep that directory.

To turn a plugin off without removing it, use `claude plugin disable <plugin>@broodhall`.

To remove the marketplace as well:

```
claude plugin marketplace remove broodhall
```

## What guardrails leaves behind

guardrails keeps a few files outside its data directory. Uninstalling does not remove them.

- `~/.local/state/guardrails/` (or `$XDG_STATE_HOME/guardrails/`): `pii-key`, the key for
  [PII](../guardrails/pii.md) tags, and `config-gate/`, the
  [config gate](../guardrails/config-gate.md) snapshots.
- `~/.config/guardrails/` (or `$XDG_CONFIG_HOME/guardrails/`): `secret-paths` and
  `pii-columns.json`, if you created them.
- The same files under `claude-tools/` in place of `guardrails/`, from before the rename.

Delete those directories to remove them.

## Extra steps for memory

Uninstalling memory does not touch two things.

- **The daemon.** It is a detached background process and keeps running. Stop it with
  `pkill -f 'memory serve'`. Until you do, it keeps reading new transcripts and sending
  tool calls to Claude to write notes (see [Privacy and redaction](../memory/privacy.md)).
  Disabling the plugin does not stop it either.
- **Your stored memories.** They live outside the plugin, in `~/.local/share/memory/` (or
  `$MEMORY_DIR`, or `$XDG_DATA_HOME/memory` when that is set). Delete that directory to
  remove them. The optional settings file in `~/.config/memory/` is also yours to delete.

Stop the daemon first, then delete the directory.

## Extra steps for rtk

Uninstalling the plugin leaves the rtk binary. If you turned on the guardrails
[`rtk` option](../guardrails/options.md#rtk), turn it off first with
`/plugin configure guardrails@broodhall`. Then remove the binary with
`cargo uninstall rtk`, if you installed it with cargo as on the [RTK](../rtk/index.md) page.
