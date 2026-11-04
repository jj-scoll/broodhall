# Git

<span class="badge badge-block">BLOCK</span> a force-push.
<span class="badge badge-ask">ASK</span> before a git command that throws away work.
<span class="badge badge-ask">ASK</span> before a git command that skips hooks or commit signing.

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

### Skipped hooks and signing: ASK

A hook that fails or a signing error is a check the repository's owner set up. Claude should
fix the cause, not step around it. These ask, and you decide.

| Command | Why it asks |
| --- | --- |
| `git commit --no-verify` or `-n`, `git am --no-verify` or `-n` | skips the pre-commit, commit-msg and applypatch hooks |
| `git push --no-verify`, `git merge --no-verify`, `git rebase --no-verify`, `git pull --no-verify` | skips the pre-push, pre-merge-commit and pre-rebase hooks |
| `git -c core.hooksPath=...`, `git config core.hooksPath ...`, `GIT_CONFIG_PARAMETERS` or `GIT_CONFIG_COUNT` with `GIT_CONFIG_KEY_n=core.hooksPath` | turns hooks off or points git at hooks outside the repository (see below) |
| `HUSKY=0`, `HUSKY_SKIP_HOOKS=1`, `LEFTHOOK=0`, `SKIP=...` before a `git commit`, `push`, `merge`, `rebase`, `am`, `cherry-pick`, `revert` or `pull` (also set with `env` or `export`) | switches Husky, Lefthook or pre-commit off |
| `--no-gpg-sign`, `git tag --no-sign`, `git -c commit.gpgsign=false`, `git config commit.gpgsign false` | turns signing off (also `tag.gpgsign` and `push.gpgsign`). `-c` counts only on commands that make a commit or tag: `commit`, `merge`, `rebase`, `cherry-pick`, `revert`, `am`, `pull`, `tag`. |

```text
ASK: git commit --no-verify skips the repository's hooks. A failing hook or a signing error is the repository's own check; fix the cause, or confirm with the user before skipping it.
```

A `core.hooksPath` asks when it turns hooks off or moves them out of the repository: an empty
value, anything under `/dev` (such as `/dev/null`), `~` or `$VAR`, an absolute path outside
the working directory, or a relative path that climbs out with `..`. A directory inside the
repository passes, because it is the normal husky and githooks setup: `git config
core.hooksPath .husky`, `.githooks`. `git config --unset core.hooksPath` passes too, since it
puts the default hooks back.

The short `-n` counts only where git means `--no-verify` by it: `git commit` and `git am`. For
`git push` it is `--dry-run`, for `git merge`, `rebase` and `pull` it is `--no-stat`, and for
`git revert` and `git cherry-pick` it is `--no-commit`. None of those ask.

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
- `git log -n 5`, `git push -n`, `git merge -n`, `git revert -n HEAD`, `git clean -n`.
- `git commit -S`, `git config core.hooksPath` and `--get` (reads), `git config --unset
  core.hooksPath`, `git config commit.gpgsign true`, `HUSKY=1`, `SKIP=` with no value, and
  `HUSKY=0 npm ci` or `HUSKY=0 git status`, which run no hook.
- Text that only mentions a command: `echo "run git reset --hard to undo"`, a commit message,
  a heredoc being written to a file, `git commit -m "use --no-verify to skip hooks"`.

If a command cannot be parsed, the destructive-verb and skipped-hook checks read it as loose
words instead and still ask, so a broken quote does not slip a `git reset --hard` or a
`git commit --no-verify` through.

Not covered:

- A git alias that holds `commit -n`, or a shell alias or variable that stands in for `git`.
- A shortened long option: git accepts any unique prefix, so `--no-v` and `--no-verif` pass.
- Edits to `.git/hooks` files or `.git/config` made with other tools.
- A hooks path whose value comes from a variable inside a script, or a setting passed some
  other way than `-c`, `git config` and the environment variables above.
- `PRE_COMMIT_ALLOW_NO_CONFIG` and other switches of single hook tools beyond Husky,
  Lefthook and pre-commit.

## Turning it off

The two halves differ.

- The destructive local commands and the skipped-hook asks are the `git` family. Add `git` to `disabled_guards` (or to
  `CLAUDE_TOOLS_OFF`, a comma-separated list) and they run without asking. There is no
  separate switch for the skipped-hook asks. The plugin's
  default list is empty, so they ask until you change it.
- The force-push block cannot be turned off.

See [Options](options.md).
