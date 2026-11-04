# Egress

<span class="badge badge-ask">ASK</span> before a command sends a secret, an outside email address or a phone number off the machine.

!!! why "Why it exists"
    A `gh` comment, a `curl` upload or an `aws s3 cp` hands text and files to a server you do
    not control, and you cannot take them back. Most of these calls carry nothing sensitive,
    and a prompt on every one would teach you to approve without reading. So the guard reads
    what is about to be sent and asks only when it holds a secret, an email address that is
    not yours, or a phone number, or when it cannot read the payload at all.

## What it catches

The guard looks at commands that send data off the machine:

- `gh` writes: `gh pr create|comment|edit|review`, `gh issue create|comment|edit`,
  `gh release create|edit|upload`, `gh gist create|edit`, and any `gh api` call.
- `curl` and `wget`.
- `aws s3 cp`, `mv` and `sync` that upload a local path to an `s3://` destination.
- MCP tools whose name has a write verb in it (`send`, `create`, `update`, `add`, `reply`,
  `forward`, `upload`, `comment`, `post`, `edit`, `transition`, `batch`, `draft`).

It scans what the call carries: the command text (heredoc bodies included) and the local
files it uploads, such as a `--body-file`, `curl -d @file`, `-F key=@file`, `-T`,
`gh api --input`, release assets, gist files and the source of an `s3 cp`. For an MCP tool it
scans every string in the input, and a local file named under a key that has `path` or `file`
in it.

It asks when the scan finds:

| Finding | Asks when |
| --- | --- |
| A secret | the text or file holds a token or key in a known shape |
| An outside email address | the address is not yours (see below) |
| A phone number | the text holds a North American style number such as `555-123-4567` |
| A payload it cannot read | stdin fed by a pipe, a directory, a file over 4 MiB, a device or pipe file, or a file that does not exist yet and is not written from a heredoc, `echo` or `printf` in the same command |

```text
ASK: this call sends data off the machine and the command text holds an outside email address (@acme-corp.test). Confirm with the user before running it.
```

The reason names the address's domain, never the address. The other findings read:

```text
ASK: this call sends data off the machine and the command text holds a phone number. Confirm with the user before running it.
ASK: this call sends data off the machine and the file /home/you/proj/notes.md holds a secret (GitHub token ghp_..6789). Confirm with the user before running it.
ASK: this call sends data off the machine and curl uploads its stdin, fed by a pipe the guard can't read. Confirm with the user before running it.
ASK: this call sends data off the machine and uploads /home/you/proj/missing.csv, which doesn't exist yet and isn't written from text in this command, so the guard can't read it. Confirm with the user before running it.
ASK: this call sends data off the machine and uploads the directory /home/you/proj, which the guard doesn't scan. Confirm with the user before running it.
```

An MCP write tool gets the same kind of reply with its name in place of "the command text":
`... and the mcp__mail__send_message input holds an outside email address (@acme-corp.test).`
MCP tools reach this guard through the plugin's `mcp__.*` hook matcher.

## What passes

- Addresses that count as yours: the domain of your global `git config user.email` (and its
  subdomains), and anything in the `allowed_email_domains` option, either full addresses or
  `@domain` entries. See [Options](options.md).
- `noreply` addresses, the local part `git`, the `example.com`, `example.org` and
  `example.net` domains, and Teams chat-thread ids.
- A `gh`, `curl` or `wget` call with no address, number or secret in what it sends, and a file
  upload whose file holds none: `curl https://example.com`,
  `gh pr create --title t --body "plain text"`, `gh issue comment 5 --body-file notes.md` with a
  clean `notes.md`.
- `gh` reads such as `gh pr view`, and MCP tools whose name has no write verb.
- `git push`: it sends commits, which you review as diffs, so this guard does not scan it.
  (Session links are a separate check: see [Session URLs](session-urls.md).)

This is a speed bump on the common routes, not a wall. It does not parse `aws s3api`, or a
send hidden inside `bash -c` or a script, and only email addresses and phone numbers count as
personal data here.

## Turning it off

Add `egress` to `disabled_guards` (or to `CLAUDE_TOOLS_OFF`, a comma-separated list). The
plugin's default list is empty, so this guard is on until you
change it. To keep it on and stop it asking about your own addresses, set
`allowed_email_domains` instead. See [Options](options.md).
