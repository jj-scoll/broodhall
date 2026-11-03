# Session URLs

<span class="badge badge-block">BLOCK</span> a `git push` or `gh pr create|edit` that would send a Claude session link off the machine.

This guard is **off by default**: the plugin's default `disabled_guards` list includes
`session-url`. See [Turning it off](#turning-it-off) for how to turn it on.

!!! why "Why it exists"
    A Claude Code session link, or a `Claude-Session` trailer in a commit message, ties a
    public commit or pull request to a private session. You may not want that published,
    and Claude Code's own commit and PR attribution guidance can tell Claude to add one. A
    check on what Claude writes would miss the routes the text arrives by (a heredoc, a body
    file, an editor, a subagent), so this guard checks at the boundary instead: the push or
    the pull request.

## What it catches

A session link is a `claude.ai/code` URL whose `session_` ID is 16 or more letters and digits.
The guard blocks these:

- `git push` (also `git -C dir push` and `cd dir && git push`) when an unpushed commit
  message carries a link or trailer. "Unpushed" means a commit on no remote ref. The reply
  names the commit hashes, never the ID.
- `gh pr create` and `gh pr edit` when the command text (flags and heredoc bodies) or a
  `--body-file` or `-F` file carries a link.
- Any of those commands whose own text carries a link, so a single command that commits and
  pushes is caught too.

```text
BLOCKED: this command carries a claude.ai session link or Claude-Session trailer (in the command text or body) and would send it off this machine. Remove it and run again. …
```

Both replies end with one more sentence that refers to the author's own `CLAUDE.md` rules;
the `…` marks where this page cuts it. For a push, the reply is:

```text
BLOCKED: unpushed commit(s) 5d42c08 carry a claude.ai session link / Claude-Session trailer. Rewrite them before pushing: `git commit --amend` for HEAD, or `git rebase --exec` / `git filter-branch --msg-filter` for older ones (git rebase -i is unavailable here). …
```

The hash is the offending commit's, so it differs. A blocked call does not run; Claude removes the
link, or rewrites the commit, and tries again.

## What passes

- A link in a command that is not a push or a PR create or edit, such as an `echo` or a file
  write.
- Text that only names the shape, with no real-looking ID: the pattern asks for 16 or more
  letters and digits after `session_`, so a rule that says "never write a Claude-Session line"
  or a placeholder such as `session_<id>` passes.
- Commits already on a remote ref. The guard does not recheck history you have already
  published; rewriting shared history is a separate decision.
- The GitHub MCP pull-request tools, a push hidden inside `bash -c`, and cloud sessions, which
  run no user hooks. The guard covers Bash only.

## Turning it off

The family is `session-url`. It is already off in the plugin's default list
`pii,raw-client-data,session-url`. To turn it on, remove `session-url` from the
`disabled_guards` option (or from `CLAUDE_TOOLS_OFF`, a comma-separated list). See
[Options](options.md).
