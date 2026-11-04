# Git

<span class="badge badge-block">BLOCK</span> a force-push.
<span class="badge badge-ask">ASK</span> before a git command that throws away work.

!!! why "Why it exists"
    A force-push rewrites a shared branch on the remote, and other people's work can vanish
    with it. `git reset --hard`, `git checkout .`, `git clean -f` and `git branch -D`
    discard changes or commits that git keeps no other copy of. The force-push is a hard
    block because there is rarely a case where Claude should do it unasked. The local
    commands ask instead, because they are often what you want and you only need to see
    them before they run. The force-push check reads the push's own flags, because the safer
    `--force-with-lease` refuses to overwrite work you have not seen, and the earlier text
    match blocked it anyway.

## What it catches

### Force-push: BLOCK

A `git push` with a bare `--force` or `-f`, in any position (`git push origin --force`,
`git -C ../repo push -f`, `git push -uf origin x`), also inside `sudo`, `bash -c` and `ssh`.
The call does not run.

```text
BLOCKED: 'git push --force' is a high-risk destructive operation. Confirm with the user before attempting this.
```

A short `-f` answers `BLOCKED: 'git push -f ' is a high-risk destructive operation. ...`.
The force-push block is a core block and has no switch.

### Destructive local commands: ASK

Claude Code shows the reason and you approve or refuse. Nothing is blocked and nothing passes
silently.

| Command | Why it asks |
| --- | --- |
| `git reset --hard` | discards uncommitted changes and moves the branch |
| `git checkout .` and `git restore .` | overwrites every uncommitted change in the directory |
| `git clean -f` (`-fd`, `--force`) | deletes untracked files for good |
| `git branch -D`, or `-d` with `-f` | force-deletes a branch, even one with unmerged commits |
| `git stash` (bare, or with a flag), `git stash drop`, `git stash clear` | moves uncommitted changes out of the working tree, or deletes saved stashes for good |

```text
ASK: git reset --hard discards uncommitted changes and moves the branch. Confirm with the user before running it; prefer a reversible step (git stash push, a backup branch, git status first).
```

The other asks differ only in the first sentence, for example
`ASK: git clean -f deletes untracked files for good. Confirm with the user before running it; ...`.

## What passes

- `git push origin main`, `git push -o ci.skip origin x`, and
  `git push --force-with-lease origin main`, with or without `--force-if-includes`. A push
  that carries `--force-with-lease` and also a bare `--force` still blocks.
- `git reset --soft HEAD~1`, `git reset HEAD file.go`, `git checkout main`,
  `git checkout -b feature`, `git checkout -- file.go`, `git restore file.go`.
- `git restore --staged .`, which only unstages.
- `git clean -n`, `git clean -fdn` and `git clean --dry-run -f`, which delete nothing.
- `git branch -d merged-feature`, `git branch -a`, `git branch --show-current`.
- `git stash pop`, `git stash list`, `git stash show -p`, `git stash apply`.
- Text that only mentions a command: `echo "run git reset --hard to undo"`, a commit message,
  a heredoc being written to a file.

If a command cannot be parsed, the destructive-verb check reads it as loose words instead and
still asks, so a broken quote does not slip a `git reset --hard` through.

## Turning it off

The two halves differ.

- The destructive local commands are the `git` family. Add `git` to `disabled_guards` (or to
  `CLAUDE_TOOLS_OFF`, a comma-separated list) and they run without asking. The plugin's
  default list is empty, so they ask until you change it.
- The force-push block cannot be turned off.

See [Options](options.md).
