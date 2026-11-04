# Shared checkout

<span class="badge badge-ask">ASK</span> before a sweeping git command takes in an uncommitted change that another live Claude session made.

This guard is **on by default**, like every family. See [Turning it off](#turning-it-off).

!!! why "Why it exists"
    Several Claude Code sessions often work in one checkout. `git add -A` stages every dirty
    file, so one session can commit a line that another session is still writing. Nothing
    in git says whose change is whose. The guard knows, because each session's file edits are
    recorded, and it asks before the sweep.

## What it catches

A command that takes in the whole tree, or the whole current directory, when at least one file
it would take in meets all of these:

- Another session wrote it with the Write, Edit, MultiEdit or NotebookEdit tool.
- That session is live: it showed a sign of life in the last 30 minutes.
- It is dirty now, according to `git status` in the command's own repository.

| Command | Takes in |
| --- | --- |
| `git add -A`, `--all`, `.` | every changed and new file, in the tree (`-A`) or the current directory (`.`) |
| `git add -u` | every changed tracked file |
| `git commit -a`, `-am`, `--all` | every changed tracked file |
| `git stash`, `stash push`, `stash save` | every changed tracked file, and new files with `-u` or `-a` |
| `git checkout .`, `git checkout -- .`, `git restore .` | every changed tracked file in the current directory |
| `git reset --hard`, `git checkout -f`, `git switch -f` (`--discard-changes`) | every changed tracked file |
| `git clean -f` | new files |

A `git stash push -- path` or `git add -A path` is limited to that path, so it asks only if
the other session edited a file under it. `git add path`, `git commit`, `git stash list`
and `git stash pop` are not sweeps and never ask here.

```text
ASK: git add -A would take in uncommitted changes that another live Claude session made in this checkout: session 3c9f1a2b (active 4 min ago): src/api.go, src/api_test.go. Confirm with the user before running it; stage or stash only your own files by path instead.
```

The reason names the other session by the first 8 characters of its id and lists up to 5
files, with a count of the rest. If two sessions are involved, both are named. The command is
found inside `bash -c`, `sudo`, `&&` chains and `git -C dir`, and a `cd dir &&` before it is followed.

When the `git` family is also on, `git stash`, `reset --hard`, `checkout .`, `restore .` and
`clean -f` have their own [ask](git.md). If another session is involved, you get this reason
instead of that one.

## How it knows

The `record-edit` hook runs after each Write, Edit, MultiEdit and NotebookEdit. It appends the
file's real path to `<session id>.edits` in the plugin's state directory. That is one small
append, with no git call. Entries older than a day are dropped when the file passes 64 KB.

A session counts as live when the newest of these is under 30 minutes old: its edits file, its
guard state, its [guard ledger](receipt.md) and its read-dedup file in `/tmp` (touched by each
Read). Only sessions that recorded an edit are considered.

The check is cheap when nothing is shared. With no other live session that has recorded
edits, no git process runs. Otherwise it runs `git rev-parse --show-toplevel` and one
`git status` limited to the files the other sessions edited, with a 1.5 second limit. If more than 300 files were recorded, the status is not limited to them. If git is
slow, missing or fails, the command goes through.

## Not covered

- Changes made through Bash: `sed -i`, a redirect, a formatter, `go generate`. They are not
  recorded, so a file only changed that way never counts.
- A file edited by a session that ended or was idle for over 30 minutes.
- A file edited by two sessions that you stage on purpose. The ask repeats each time.
- A linked worktree is a different checkout. Edits there never count for the main
  checkout, and the reverse.
- Files outside the repository, and files git ignores.
- Subagents share their parent's session id, so they never count as another session.
- Commands that point git elsewhere: `--git-dir`, `--work-tree`, `GIT_DIR`, `GIT_WORK_TREE`.
- Text the shell parser rejects. `git clean -f` without `-d` is checked as if it also removed
  new directories, so it may ask when it would not have taken the file.
- `git commit` of a change another session already staged, and `git add -p`.
- A glob or magic pathspec (`:(exclude)`, `*.go`) counts as the whole tree. `git add :/`, `..` and `*` are read as the whole tree; an absolute path to the repository root is not.
- A file edited through a symlink, and a file renamed after the other session edited it: only the path written is recorded.
- Git aliases, a command built from variables or `$(...)`, and `git commit -a` of a change that is only staged.
- Many `record-edit` hooks racing while the edits file is pruned (past 64 KB) can lose recent entries.
- File names with control characters are shown quoted.

## Turning it off

The family is `sweep`. Add it to the `disabled_guards` option (or to `CLAUDE_TOOLS_OFF`, a
comma-separated list). Recording continues, but nothing asks. In [observe mode](options.md#observe)
the ask is let through and logged as `would ask (shared-tree-sweep)`. See [Options](options.md).
