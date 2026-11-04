# Settings files

<span class="badge badge-ask">ASK</span> before Claude changes a Claude Code settings file.

!!! why "Why it exists"
    Claude Code's settings files hold the hooks and permission rules that every other guard
    depends on. A model that can rewrite them can switch the guards off, or grant itself the
    permissions it was refused. So a change to one is something you see first, even when an
    allow rule in the settings would have let the write straight through.

## What it catches

The target is `settings.json` or `settings.local.json` in one of these:

- a project's `.claude` directory;
- the session's `CLAUDE_CONFIG_DIR`, whatever its name;
- a directory right in your home whose name starts with `.claude`, such as `~/.claude`,
  `~/.claude-work` or `~/.claude-personal`.

The last two are the Claude config directories, the same ones whose credentials the
[Secret files](secret-files.md) guard protects. A command may name the session's as
`$CLAUDE_CONFIG_DIR`. A config directory with another name that this session does not use
is not covered.

A relative path counts from where the command started. Once a `cd`, `pushd` or `popd` shows
up anywhere in the command (also `builtin cd`, `command cd`, `\cd` or `if cd`), or `CDPATH`
does, the guard can't be sure where it runs. From there on, a relative `.claude*` path is
also read as one in your home. `.claude-plugin` is left out of that, since every plugin repo
has one. When a segment starts with `cd` or `pushd` and a plain path, such as `cd /x` or
`cd ~/.claude-work`, relative paths after it are also read from there. A write inside a
`( ... )` subshell or `bash -c '...'` is not seen.

- **Edit, Write, MultiEdit and NotebookEdit** on one of those files. Symlinks are resolved,
  so a link that points at one counts.
- **Bash writes** to one: a redirect (`echo x > ~/.claude/settings.json`), `sed -i`, `tee`,
  `cp` or `mv` onto it, `rm`, `chmod`, an editor, or a script interpreter such as `python`
  whose command names the file next to a write. `find` counts when it runs `-delete` or a
  `-exec` of a writing command.

The reply is the same for every route:

```text
Claude Code settings files hold the hooks and permissions that guard every session. The user confirms each change the model makes to one.
```

The hook asks (`permissionDecision: ask`); the call waits for you and nothing is blocked
outright.

## What passes

- Reading: `cat ~/.claude/settings.json` and `ls ~/.claude`.
- Edit or Write on any other file, such as `/tmp/notes.md`.
- `rmdir` of a config directory, which removes only empty directories, and `find` that
  neither deletes nor runs a writing command.

This is a speed bump, not a wall. The Bash check reads the command text, so a script written
earlier or a path assembled from variables gets past it.

## Turning it off

You cannot. This guard has no family name in `disabled_guards`, and the code never consults
`GUARDRAILS_OFF` for it: with `pii`, `raw-client-data`, `session-url`, `egress`, `git` and `sweep`
all off, an Edit of `~/.claude/settings.json` still asks. You can approve each ask when it
appears. See [Options](options.md).
