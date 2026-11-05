# Privacy and redaction

## Do transcripts leave the machine?

Yes, in one place. To write each note, the daemon runs `claude -p` as a child process, with
`--model claude-haiku-5-5`, and passes the tool call on its standard input. That is the same
`claude` command you use, on your `PATH`, signed in as you. The request goes to Anthropic
like any other Claude request, and the usage counts against your account.

The same command writes the session summaries, and for those it sends your prompts and the
titles of the session's notes (see [What is sent](#what-is-sent)).

Nothing else is sent anywhere. The daemon's own code makes no outbound network request. The
database, the UI and the Quack server stay on your machine (see
[UI and Quack](ui-and-quack.md)). The only other download is the program itself, from this
repository's GitHub release, described in [Install](../guide/install.md).

The child process is started with no tools, no MCP servers, no slash commands, no settings
sources and no saved session. It runs in its own directory, `observer-cwd`, under the state
directory, and with `MEMORY_OBSERVER=1` in its environment so it can be recognised.

## What is sent

For each queued tool call:

- the tool name,
- the tool input, and
- the tool output.

Each of the three is masked first (below), then cut to its first 6,000 and last 2,000
characters if it is longer. The cut happens after masking, so it cannot expose half of a
secret.

For each session summary:

- your last 30 prompts in that session, each cut to its first 1,500 and last 500 characters
  if it is longer, and
- the titles of the session's last 100 notes, each cut to 200 characters.

Both are masked first, then cut. Nothing else from the session goes into a summary request.

Compaction summaries are not sent anywhere. The `memory` program masks them before it writes
them to the `compactions/` folder in the state directory, and the daemon masks them again
before they are stored. Only masked text reaches the database or Claude.

Some files can hold the summary unmasked. The same text is already in your Claude Code
transcript. The first three are in the `compactions/` folder and are mode 0600:

- `compactions/raw-*.json`: the hook launcher saves the raw payload when the `memory` program
  is not installed yet, fails, or is stopped for taking too long. The daemon masks it when it
  reads it and then deletes the file. The file stays while the daemon is not running, or if
  the database returns an error.
- `compactions/rejected/`: a file the daemon cannot use (for example a session id with odd
  characters) is moved here as it is, so it can still hold unmasked text. Nothing in this
  folder is ever stored or shown. The daemon deletes entries older than 7 days.
- `compactions/.*.tmp`: a half-written file left by a hook that was stopped. The daemon
  deletes these after 1 hour, while it is running.
- `memory-hook.*/in` in your temporary directory: the launcher copies the payload of every
  hook there (the folder is mode 0700) and removes it when the hook ends. If the hook is
  killed hard, the copy stays until the system cleans the temporary directory.

Tools in the skip list are never sent. By default these are `listmcpresourcestool`,
`slashcommand`, `skill`, `todowrite` and `askuserquestion`. You can change the list, and add
a pattern that skips matching Bash commands, in [Options](options.md).

## What is masked

Redaction runs when a transcript line is read, before anything is stored, and again before
the payload is built. It replaces secret-shaped strings, such as API keys and tokens, and the
value after a secret-looking key, such as `password=...` or `token: ...`, with a tag. The
same input always gets the same tag, and masking twice changes nothing more.

User prompts, tool inputs, tool outputs and text saved by `save_memory` or `work_state_write`
all pass through it.

## What redaction does not do

It matches patterns. It is not a data-loss filter.

- It does not find personal data such as names, addresses or customer records. For those,
  see the PII guard in [guardrails](../guardrails/pii.md), which is a separate plugin.
- An unquoted value after a secret key is masked up to the first space. A passphrase of
  several words has its first word masked and the rest left readable, unless a secret
  pattern catches it.
- A number after a plural count word such as `tokens` or `keys` (`input_tokens: 1994`) is
  kept, because these are almost always counts.
- Code, file paths and prose are not touched. If a file you read holds something sensitive
  but not secret-shaped, it can be stored and sent.

## How to send less

- Skip a tool or a Bash pattern with the settings file ([Options](options.md)).
- Stop the daemon, or uninstall the plugin ([Uninstall](../guide/uninstall.md)). With the
  daemon stopped, nothing is read and nothing is sent.
- Delete the state directory to remove what is already stored.

## What is stored

The database holds your prompts, tool inputs and outputs (masked), and the notes and
summaries. It is a file in the state directory. For each file a note names, it also holds a
hash of that file's content at the time (see [Stale notes](index.md#stale-notes)); the hash
is not sent anywhere. Treat it like your transcripts, which it mirrors.
