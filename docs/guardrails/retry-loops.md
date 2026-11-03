# Retry loops

<span class="badge badge-note">NOTE</span> on the third failure of the same Bash command.

!!! why "Why it exists"
    When a command fails, running it again unchanged rarely works, and a loop of identical
    retries spends time and tokens without learning anything. The note comes at the third
    identical failure and tells Claude to stop, say what it knows, and try a different
    approach. A run of different failing commands is left alone, because that is
    exploration, not a retry loop.

## What it catches

The guard counts failed Bash commands. A command is the same as an earlier one when it is the
same after whitespace is collapsed (`make   test` and `make test` match), looking at its first
300 characters.

On the **third** failure of one command in a session, Claude gets this note:

```text
This exact command has now failed 3 times this session. Exploration budget: stop retrying, report what is known, then change search strategy or name the missing evidence. Retrying the same call again is not a new approach.
```

- The note comes once, at the third failure. A fourth and a fifth failure of the same command
  say nothing more.
- The count is per session, and each subagent counts separately from the main session.
- The failures need not come one after another. Other commands between them do not reset the
  count.
- A success of that command clears its count.
- Nothing is ever blocked: the command runs as before, and the call that failed has already
  happened. An interrupted command is not counted.

## What passes

- Different commands failing in a row (`ls a`, `ls b`, `ls c`): each has its own count of one.
- A command that fails twice, succeeds, and fails twice again: the success cleared the count.
- Every failure after the third of the same command, until a success clears the count.
- Any tool other than Bash.

## Turning it off

You cannot. Retry loops have no family name in `disabled_guards`, and none of the five
families (`pii`, `raw-client-data`, `session-url`, `egress`, `git`) controls them. The note
never blocks. You can disable the plugin. See [Options](options.md).
