# Bash core

<span class="badge badge-block">BLOCK</span> The shell commands that destroy a machine, not a
project. These blocks have no switch.

!!! why "Why it exists"
    A few Bash commands cannot be undone and do far more damage than anything else Claude
    runs: deleting the filesystem root or your home directory, overwriting a disk with `dd`,
    a fork bomb, opening the whole tree to every user, writing into `/etc`. Approving Bash
    once covers them along with `ls`. The rules match the exact dangerous target rather
    than a piece of text, because an earlier, looser text match also stopped ordinary
    cleanups of scratch directories.

## What it catches

The call does not run. The hook exits with status 2 and the message goes back to Claude.

| Command | Blocked when |
| --- | --- |
| `rm` | It is recursive and forced (`-rf`, `-fr`, `-r -f`, `--recursive --force`) and a target is exactly `/`, `~`, `$HOME`, your home directory, `/home`, `/nix` or `/storage`. A trailing `/` or `/*` counts as the same target. |
| `dd` | Any `if=` argument. |
| Fork bomb | The `:(){ :|:& };:` form. |
| Redirect | Output (`>`, `>>`, `&>`) written to a path under `/etc/`. |
| `chmod` | Recursive with mode `777`, `0777`, `a+rwx` or `ugo+rwx` on one of the same protected targets as `rm`. |
| `chown` | Recursive on one of the same protected targets as `rm`. |
| `docker` / `podman` | `run` or `create` with `--privileged` or a mount of `/dev`. |

The rules read the parsed command, so they also see these inside `sudo`, `cd x && ...`,
`bash -c '...'` and `ssh host '...'`.

```text
BLOCKED: 'rm -rf ~' is a high-risk destructive operation. Confirm with the user before attempting this.
```

The reply names the pattern that matched, so `rm -rf /*` answers
`BLOCKED: 'rm -rf /' is ...` and `rm -rf /home` answers `BLOCKED: 'rm -rf /home' is ...`.
These are the other replies, each captured from the guard binary:

```text
BLOCKED: 'dd if=' is a high-risk destructive operation. Confirm with the user before attempting this.
BLOCKED: '> /etc/' is a high-risk destructive operation. Confirm with the user before attempting this.
BLOCKED: 'chmod -R 777 /' is a high-risk destructive operation. Confirm with the user before attempting this.
BLOCKED: 'chown -R' is a high-risk destructive operation. Confirm with the user before attempting this.
BLOCKED: ':(){ :|:& };:' is a high-risk destructive operation. Confirm with the user before attempting this.
```

A privileged container gets a longer reply that begins:

```text
BLOCKED: privileged container or host /dev mount. …
```

## What passes

Only the exact targets above block. These all run:

- `rm -rf /tmp/build`, `rm -rf ~/.cache/some-tool`, `rm -rf node_modules`. A path below a
  protected target is not the target.
- `rm -rf /usr` and `rm -r /`. The first names a directory that is not in the list; the
  second is not forced. Neither is caught.
- `echo rm -rf /`, `grep "dd if=" notes.md`, a commit message that mentions a command. Text
  that is not an invocation is not read as one.
- `dd of=out.img` (no `if=`), `cat /etc/hosts` (a read), `chmod -R 777 /var/www/html`,
  `chown -R user /srv`, `docker run -v /home/user/dev:/src ubuntu`.

If a command cannot be parsed, for example it has an unterminated quote, the guard falls back
to the older text match for that command: `echo "don't; rm -rf /` blocks. A `docker` or
`podman` command is checked by running the rules on the command it runs inside the
container, with bind mounts mapped back to host paths: `docker run -v /:/host ubuntu rm -rf
/host` blocks as `rm -rf /`, while `docker run ubuntu rm -rf /` (the container's own root)
runs.

## Turning it off

You cannot. `disabled_guards` accepts `pii`, `raw-client-data`, `session-url`, `egress` and
`git`, and none of them touches these rules. A test confirms that `rm -rf ~` still blocks
with all five off. To get past a block, run the command yourself in a terminal, or disable
the plugin. See [Options](options.md).
