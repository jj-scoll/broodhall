# Privacy and redaction

## Do transcripts leave the machine?

Yes, in one place. To write each note, the daemon runs `claude -p` as a child process, with
`--model claude-haiku-5-5`, and passes the tool call on its standard input. That is the same
`claude` command you use, on your `PATH`, signed in as you. The request goes to Anthropic
like any other Claude request, and the usage counts against your account.

The same command writes the session summaries, and for those it sends your prompts and the
titles of the session's notes (see [What is sent](#what-is-sent)).

`memory import` (see [Import past sessions](index.md#import-past-sessions)) is local. It reads
old transcripts into the database, sends nothing, and never starts `claude`. The history it
adds is not queued for notes and gets no summary. If you later continue an old session, the
summary written for it can include that session's imported prompts, as for any session.

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

User prompts, Claude's replies, tool inputs, tool outputs and text saved by `save_memory` or
`work_state_write` all pass through it. Calls described in [What is never kept](#what-is-never-kept) lose their
content first, then what is left is masked.

## What is never kept

Some tool calls are kept without their content, whatever it looks like. This runs before
anything is stored, so the stored row and the text sent to Claude are the same.

- **Credential files.** A tool call that reads a credential file is kept without its
  output. This covers `Read` and `Grep` on the file, and a `Bash` or `PowerShell` command
  that names it. A call that writes one (`Write`, `Edit`, `MultiEdit`, `NotebookEdit`) is
  kept with the path only, not what it wrote. The list is the one the
  [guardrails secret-file guard](../guardrails/secret-files.md) uses: `.env` and `.env.*`
  (not `.env.example`, `.env.sample` or `.env.template`), `*.tfvars`, `*.pem`, `.npmrc`,
  `~/.ssh/id_*` (not the `.pub` files), `~/.aws/credentials`, `~/.kube/config`,
  `~/.netrc`, `~/.docker/config.json` and the rest of that page's list. Lines you add to
  guardrails' `secret-paths` file are not read here.
- **Credential commands.** A Bash command whose output is a credential is kept without its
  output. The list is short and fixed: `gh auth token`, `kubectl get secret`,
  `aws configure get *secret*` and `*session_token`, `aws sts get-session-token`,
  `aws sts assume-role`, `aws secretsmanager get-secret-value`,
  `aws ssm get-parameter --with-decryption`, `aws ecr get-login-password`,
  `gcloud auth print-access-token` and `print-identity-token`,
  `az account get-access-token`, `op read`, `vault read`, `vault kv get` and
  `security find-generic-password -w`.
- **Excluded directories.** A session that runs in a directory listed in
  `capture_exclude_dirs` (or in a directory under it) keeps only the tool name of each
  call, and whether it errored. Your prompts there are still kept, masked as usual.
- **`capture_tools` off.** Every call in every session keeps only the tool name and
  whether it errored. Prompts are still kept.

Where the output is dropped, the stored output is `[output not kept: credential file]` or
`[output not kept: credential command]`, and the call is still sent for a note, with that
text in place of the output. A call kept by name only has an empty input, and its output is
empty, or `[error]` if the tool failed. It is not sent for a note, because there is nothing
to write about.

The check reads the text of the command. It finds a file named in the command, also through
`~`, `$HOME`, a quoted word, a pipe, `bash -c`, a `NAME=.env` assignment and a glob such as
`.env*`. It cannot find a path built at run time from a variable the command does not set,
or the file read by a program you wrote earlier. It errs towards dropping: a command that
only mentions `.env` in a commit message loses its output too.

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
- Keep only tool names for a directory with `capture_exclude_dirs`, or for everything with
  `capture_tools` (see [Options](options.md)).
- Stop the daemon, or uninstall the plugin ([Uninstall](../guide/uninstall.md)). With the
  daemon stopped, nothing is read and nothing is sent.
- Delete the state directory to remove what is already stored.

## What is stored

The database holds your prompts, Claude's replies, tool inputs and outputs (all masked, and
left out for the calls in [What is never kept](#what-is-never-kept)), and the notes and
summaries. It is a file in the state directory. A reply is the text Claude
wrote to you, one row per text block. Thinking blocks are not stored. Replies from
subagents and from the observer are not stored. Replies are masked as they are read, like
prompts, and are never sent anywhere: the observer and the summary writer do not see them.
Each session also keeps the git branch and the model last seen in its transcript.

The database also holds a search index over the prompts, replies, tool inputs and the file
paths of file tool calls (see [Raw search](index.md#raw-search)). It is made of the same
masked text, split into words, so it adds no new text. For each file a note names, it also holds a
hash of that file's content at the time (see [Stale notes](index.md#stale-notes)); the hash
is not sent anywhere. Treat it like your transcripts, which it mirrors.
