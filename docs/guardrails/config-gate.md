# Config gate

<span class="badge badge-block">BLOCK</span> a settings change that weakens the guards.

!!! why "Why it exists"
    The guards live in this plugin's `hooks/hooks.json`, and a settings file cannot edit that.
    But a settings file can switch the plugin off, turn guard families off through its options,
    or add hooks and permission rules that step around it. A model that can write
    `settings.json` could do any of those, so the gate compares each settings change with what
    the session started with and refuses the ones that loosen the guards.

## What it blocks

The hook runs on `ConfigChange` for the user, project and local settings files. It exits 2,
which keeps the session on its current settings, when the change does any of this:

- Sets `enabledPlugins` for `guardrails@<marketplace>` to `false`, or removes the entry.
- Adds a family to the plugin's `disabled_guards` option in `pluginConfigs`. An unset option
  counts as the default families, so unsetting an option that was empty also counts.
- Adds, changes or removes a `GUARDRAILS_*` or `CLAUDE_TOOLS_*` variable under `env`, `GUARDRAILS_OFF`
  included.
- Adds a `PreToolUse` or `PermissionRequest` hook. Such a hook can allow or rewrite a call
  the guards ask about, and a settings file cannot say which kind it is.
- Removes a `PreToolUse`, `PermissionRequest` or `ConfigChange` hook, or any hook with
  `onFailure: block`, or drops `onFailure: block` from one.
- Sets `disableAllHooks`, or sets `defaultMode` to `bypassPermissions`.
- Removes a `deny` or `ask` permission rule.

The reply lists each reason on stderr. Deleting a settings file is judged the same way, as a
change to an empty file.

## What it reports

When a change newly allows a way for data to leave the machine, the hook exits 1: you see
the message, and the change applies. Rules that do this include `Bash` with no pattern,
`WebFetch`, `Bash(curl *)`, `Bash(git push:*)`, `Bash(gh api *)`, `Bash(aws s3 cp:*)`,
`Bash(ssh *)`, interpreters such as `python` and `node`, and MCP tools whose names say send,
create, update or similar.

## How it decides

At session start the hook saves a snapshot of the user and project settings files, in
`$XDG_STATE_HOME/guardrails/config-gate/` (`~/.local/state/...` when unset). Later changes
are compared with the snapshot. A change that applies becomes the new snapshot; a blocked one
does not. Claude Code stages some edits as `<file>.proposed-<id>` for review; those are
compared with their target and never written when blocked. A direct edit is already on disk
when the hook runs, so a block keeps this session on its old settings but the next session
loads the file.

## What passes

Changes to skills and managed settings, invalid JSON (Claude Code rejects it itself), and any
edit that keeps every guard. A file the hook cannot read blocks, and so does input it cannot
parse.

## Limits

- It is a tripwire. The [Settings files](settings-files.md) ask is the lock that stops the
  edit in the first place.
- `claude plugin uninstall guardrails` changes no settings file, so the gate does not see it.
- A session started after a blocked edit takes the weakened file as its snapshot, and an
  older session then compares its next change with that file.
- Without a snapshot, such as the first change before any session start, the file is taken as
  it is.

## Turning it off

You cannot. It has no family name in `disabled_guards`. If the binary is not installed yet,
the launcher lets the change through, like every other hook.
