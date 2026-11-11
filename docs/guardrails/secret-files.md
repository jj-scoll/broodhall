# Secret files

<span class="badge badge-mask">MASK</span> a Bash read of a credentials file.
<span class="badge badge-block">BLOCK</span> the Read tool on one.
<span class="badge badge-ask">ASK</span> for any other use of one, and for Edit and Write.

!!! why "Why it exists"
    Everything Claude reads goes into the conversation, and from there into the transcript.
    A cloud credentials file or an SSH key read in plain text is a secret now sitting in
    places you do not control. Claude rarely needs the value itself, only the structure of
    the file or the tool that uses it. So a read shows the file with the values masked, and
    anything the mask cannot cover is put to you instead.

## Which path asks, and which masks

The same file gets a different answer depending on how Claude reaches it.

| How | Answer |
| --- | --- |
| Bash, a plain reader: `cat`, `head`, `tail`, `grep`, `rg`, `sed -n 'Np'`, `jq .`, `yq .` | **MASK.** The call runs, rewritten to go through `guardrails redact --secrets`. |
| Bash, anything else that names the file: `base64`, `cp`, `python`, a redirect, `find -exec` | **ASK.** |
| The Read tool | **BLOCK**, with a pointer to `cat` in Bash, which is masked. |
| Edit, Write, MultiEdit, NotebookEdit | **ASK.** |
| Bash, a command that holds a secret-shaped value (a token, a key) | **ASK.** |

So the plugin does not ask before a plain read of a secret file. It masks the Bash read and
refuses the Read tool. It asks before the routes the mask cannot cover, and before writes.

## What it protects

These files, plus the ones you add:

`~/.aws/credentials`, `~/.aws/sso/cache/*.json`, `~/.aws/cli/cache/*.json`,
`~/.ssh/id_rsa`, `id_ecdsa`, `id_ed25519` (and the `_sk` variants), `~/.netrc`, `~/.pgpass`,
`~/.docker/config.json`, `~/.config/gh/hosts.yml`, `~/.config/mark.toml`, `~/.kube/config`,
`~/.pypirc`, `~/.git-credentials`, `~/.config/gcloud/application_default_credentials.json`,
`~/.config/gcloud/credentials.db`, `.credentials.json` in every Claude config directory, and
the PII key. These files are protected by name, in any directory (a project `.env` as much
as one in your home): `.env`, `.env.*`, `.npmrc`, `.pypirc`, `.git-credentials`, `*.tfvars`
and `*.pem`. The templates `.env.example`, `.env.sample` and `.env.template` are not
protected, and neither is `*.tfvars.example`.

- A Claude config directory is the session's `CLAUDE_CONFIG_DIR`, whatever its name, or a
  directory right in your home whose name starts with `.claude`: `~/.claude`,
  `~/.claude-work`, `~/.claude-personal`. It counts even before it exists, and a command
  may name it as `$CLAUDE_CONFIG_DIR`. A config directory with another name that this
  session does not use is not covered. Add it to `secret-paths`.
- The PII key is `~/.local/state/guardrails/pii-key`, or under `$XDG_STATE_HOME/guardrails/`
  when `XDG_STATE_HOME` is set. That is where the plugin keeps it.
- `~/.config/mark.toml` is the config of [mark](https://github.com/kovetskiy/mark), a
  Markdown-to-Confluence tool. It holds an Atlassian API token in plain text.

Add a path or glob per line to `~/.config/guardrails/secret-paths` (`~` allowed, `#`
comments). With `XDG_CONFIG_HOME` set, the file is
`$XDG_CONFIG_HOME/guardrails/secret-paths`. That list adds to the defaults, and the list
file is protected too. See [Options](options.md).

## What it does

### Masked reads

For `cat ~/.aws/credentials`, the guard lets the call run but replaces the command:

```text
guardrails redact --secrets -- cat ~/.aws/credentials
```

and tells Claude why, in a note that rides along:

```text
pre-tool-use masked this read of a protected secret file (`guardrails redact --secrets`): token shapes show as [kind:tag] and values under secret-looking keys as [secret:tag]; equal values share a tag. The real values stay in the file: run the tool that uses it rather than copying values.
```

For a file holding AWS's documentation example keys, Claude sees:

```text
[default]
aws_access_key_id = [aws-access-key:6fa5b0]
aws_secret_access_key = [aws-secret-key:07646e]
```

The tags are derived per machine, so yours differ. Only flags that print whole lines keep
the mask working (the key name must stay on the line); other flags on a reader ask instead.
A recursive `grep` or `rg` of a directory that holds a protected file is masked the same way.

In auto mode a rewrite can be denied, and Claude then re-issues the rewritten command as
it is. The guard lets that through, without wrapping it again, when it is exactly its own
wrapper: this plugin's binary, `redact --secrets --`, then a reader the mask accepts. A
wrapper with another binary (even one named `guardrails`), another flag, or a reader flag
that would ask, is judged as if the wrapper were not there, and so is one behind `sudo` or
`env`. Each pipeline stage and each `&&` part is judged on its own, so a later `cat` of the
same file that is not wrapped is masked, or asks.

### Asks

```text
ASK: it reads or writes a protected secret file in a way the mask cannot cover. Plain cat, head, tail, grep, rg, sed -n 'Np' and jq . reads are masked; ls, stat, test and wc pass; to use a value without showing it, capture it: T=$(sed -n 's/^password = "\(.*\)"$/\1/p' FILE). Confirm with the user before running it.
```

That is the reply to `base64 ~/.aws/credentials`, `cp ~/.aws/credentials /tmp/c`,
`echo hi > ~/.aws/credentials` and `bash -c 'cat ~/.aws/credentials'`. A protected path
spelled out anywhere else in the command text asks too (not the by-name files such as
`.env`, which ask only when named as an argument or redirect target). Edit and Write on a protected file ask:

```text
ASK: /home/you/.aws/credentials is a protected secret file. Confirm with the user before running it.
```

A command that carries a secret-shaped value, such as a GitHub token in a header, asks:

```text
ASK: the command holds a secret-shaped value (GitHub token ghp_..6789). Prefer a file or variable the tool reads itself. Confirm with the user before running it.
```

### Read: blocked

```text
BLOCKED: /home/you/.aws/credentials is a protected secret file. Read it in Bash with `cat /home/you/.aws/credentials`: pre-tool-use masks its values.
```

The block runs in the plugin's Read hook (`large-read-guard`), not in `pre-tool-use`. See
[Read limits](read-limits.md).

## What passes

- `ls -l`, `stat`, `test`, `wc -l` and the other metadata commands on a protected file.
- `grep` or `rg` with `-l`, `-c` or `-q` (or `--files`), which print file names or counts and
  no lines.
- Output captured into a variable and not shown, such as
  `T=$(sed -n 's/^aws_secret_access_key = \(.*\)$/\1/p' ~/.aws/credentials)`. Masking
  would break the value the command goes on to use.
- Reading any file that is not protected.

This is a speed bump, not a wall. Claude runs as you, and a script written earlier or a path
built from variables gets past the check. So does a program that opens the file itself:
`cat .env` is masked, but `python3 -c "print(open('.env').read())"` is not seen, because
a file name alone in a script is not a reason to ask. A recursive `grep` or `rg` is masked
for a directory that holds a protected path from the list above (such as `~/.aws`), but
not for a directory that merely holds a `.env`; name the file.

## Turning it off

You cannot. This guard has no family name in `disabled_guards`: with `pii`,
`raw-client-data`, `session-url`, `egress`, `git` and `sweep` all off, a Bash read of
`~/.aws/credentials` is still masked and the Read tool still blocked. You can approve each
ask when it appears, or disable the plugin. See [Options](options.md).
