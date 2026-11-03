# Why

Claude Code's permission system works per tool. Allow `Bash` once and it covers `ls` and
`rm -rf ~` alike; deny it and the session stops being useful. There is no setting that says
"Bash is fine, except for these commands".

guardrails fills that gap. It is a set of hooks that read each tool call before it runs and
answer in one of four ways.

!!! why "Why it exists"
    Permission is all or nothing per tool. The commands that cost the most when they go
    wrong, such as a recursive delete of a home directory or a force-push, are a tiny
    fraction of what Bash is used for. A refusal or a prompt on only those commands costs
    less than approving everything or approving nothing.

## What it says back

Each reply below is the exact text the guard binary returned for that command, captured from
a binary built from the exported source, with the plugin's default disabled families
(`pii`, `raw-client-data`, `session-url`) off.

### BLOCK

The call does not run. The hook exits with status 2 and its message goes back to Claude.

```text
BLOCKED: 'rm -rf ~' is a high-risk destructive operation. Confirm with the user before attempting this.
```

`git push --force origin main` gets the same kind of reply. `git push --force-with-lease`
passes.

### ASK

The call waits for you. Claude Code shows the reason and you approve or refuse. This is the
reply to `git reset --hard`:

```text
ASK: git reset --hard discards uncommitted changes and moves the branch. Confirm with the user before running it; prefer a reversible step (git stash push, a backup branch, git status first).
```

### MASK

The call runs, but its output is rewritten so secret values do not reach the model. A Bash
read of a file in the protected set, here `~/.aws/credentials`, is rewritten to run through
the guardrails binary. This is what Claude sees for a credentials file holding example keys:

```text
[default]
aws_access_key_id = [aws-access-key:39581a]
aws_secret_access_key = [aws-secret-key:e9c9f3]
```

The values stay in the file. Equal values share a tag, so Claude can still tell that two
settings match.

### NOTE

The call runs, and a short note rides along for Claude. Nothing is blocked. This is the note
for `docker build -t x .`:

```text
SLOW_OP: `docker build` detected. Only run this if there is a concrete reason (e.g. verifying an image). If this is exploratory, use a cheaper check first.
```

## Design choices

- **Fails closed once installed.** The hooks that gate tool calls are configured with
  `onFailure: block`. If the guard binary fails while checking a call, Claude Code blocks
  that call rather than letting it through unchecked.
- **Never blocks on a missing binary.** If the binary was never downloaded, the launcher
  reads the call and exits 0, which allows it. A failed download turns the guards off for
  that session and shows a one-line notice. It does not stop your work.
- **One static binary.** The hooks are a single Go program with no runtime dependencies. It
  is downloaded once per plugin version and checked against a published sha256 before it is
  used.
- **Nothing leaves the machine from the guards.** The binary's dependency graph contains no
  networking packages, so the guards make no network calls of their own. The only thing in
  the plugin that touches the network is the launcher script, which fetches the release
  tarball and `checksums.txt` from this project's GitHub releases at session start, when
  the binary for that plugin version is not already on disk. The guards run `git` locally,
  to read your `user.email` and to look for unpushed commits. When a read is masked, the
  guard runs the command Claude asked for, so that command does whatever it would have done
  anyway.

## What it isn't

guardrails is not a sandbox. It reads the command text and tool arguments Claude Code hands
it and applies rules; it does not contain a process, and a command written to dodge the
rules can get past them. It is aimed at accidents and at a model that does not know a
command is dangerous, not at someone set on defeating it. You can turn guard families off in
the plugin options or disable the plugin altogether, and nothing stops you running the same
command yourself in a terminal.
