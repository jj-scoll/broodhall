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
| `rm` | It is recursive and forced (`-rf`, `-fr`, `-r -f`, `--recursive --force`) and a target is exactly `/`, `~`, `$HOME`, your home directory, `/home`, `/nix` or `/storage`. A trailing `/` or `/*` counts as the same target, and so does the same path written another way: `/home/../home/you`, `$HOME/../you`, `T=~; rm -rf "$T"`, or a glob that can match it (`/hom[e]/you`, `/h*`). |
| `dd` | It writes to a device: an `of=` under `/dev/`. An image file (`dd if=/dev/zero of=disk.img`) passes. |
| Fork bomb | The `:(){ :|:& };:` form. |
| Redirect | Output (`>`, `>>`, <code>&gt;&#124;</code>, `&>`, `&>>`) written to a path under `/etc/`. |
| `chmod` | Recursive, with any mode, on one of the same protected targets as `rm`. It rewrites every mode below the target and nothing records the old ones, so `000` or `go-rwx` is as lasting as `777`. |
| `chown` | Recursive on one of the same protected targets as `rm`. |
| `docker` / `podman` | `run` or `create` with `--privileged` or a mount of `/dev`. |

The rules read the parsed command, so they also see these inside `sudo`, `cd x && ...`,
`bash -c '...'` and `ssh host '...'`.

They also see through the commands that only run another command: `doas`, `env` (including
`env -S '...'`), `timeout`, `nice`, `ionice`, `nohup`, `setsid`, `stdbuf`, `flock`, `time`,
`command`, `builtin`, `exec`, `xargs` and `parallel`. A `-c` string given to `su`, `script`
or `flock`, the command line of `watch`, a `trap` handler and the commands of `find -exec`,
`-execdir` and `-ok` are read the same way. So `timeout 5 rm -fr /` and
`find . -exec rm -fr / {} \;` block like the bare command. A target that arrives later is not known: `echo / | xargs rm -rf`
passes. Shell commands that a `python -c` or `perl -e` one-liner hands to `os.system`,
`subprocess`, `system` or `exec` as literal strings are read too.

The command name and payload are read as the shell will run them, when the text alone fixes
them: `\rm`, `r''m`, `"rm"`, `c=rm; "$c"`, `$'\x72\x6d'`, `"$(printf '\162\155')"`,
`{rm,-rf,~}`, `rm${IFS}-rf`, a function the same line defines (`r() { rm "$@"; }; r -rf ~`),
and code piped or sourced into a shell (`printf 'rm -rf ~' | sh`,
`echo <base64> | base64 -d | sh`, `source <(printf ...)`) all block like the plain command.
When the text does not fix them, the guard asks instead (see [Asks](#asks)).

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
BLOCKED: 'chmod -R' is a high-risk destructive operation. Confirm with the user before attempting this.
BLOCKED: 'chown -R' is a high-risk destructive operation. Confirm with the user before attempting this.
BLOCKED: ':(){ :|:& };:' is a high-risk destructive operation. Confirm with the user before attempting this.
```

`chmod` answers `chmod -R 777 /` for an open mode (`777`, `a+rwx`) and `chmod -R` for any other. The `chown` reply is always this one.

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
- `dd if=/dev/zero of=out.img` (a file, not a device), `cat /etc/hosts` (a read), `chmod -R 777 /var/www/html`,
  `chown -R user /srv`, `docker run -v /home/user/dev:/src ubuntu`.
- Commands that only print or only parse: `chmod`, `chown` or `rm` with `--help` or `--version` before the
  operands (`chmod` and `chown` after them too, on Linux, unless `POSIXLY_CORRECT` is set on the command),
  `bash -n`, `sh -n` and `zsh -n` (`bash -n -c 'rm -rf ~'`), and `git push --dry-run` (`-n`) with any
  `+ref`, `--force`, `--delete` or `:ref`. `dd` and `rm -rf ~ --help` are still judged.

## Asks

<span class="badge badge-ask">ASK</span> These do not block. Claude Code shows the reason and
you approve or refuse. They have no switch.

| Rule | Asks before |
| --- | --- |
| `iac-destroy` | `terraform`, `tofu` or `terragrunt` `destroy`, and `apply -destroy`. `plan -destroy` passes, and so does `terraform destroy -help` (`-h`, `--help`), or the same with `tofu`: they print help and run nothing. |
| `s3-rm` | `aws s3 rm --recursive` and `aws s3 rb --force`. A single-key `rm` passes. |
| `find-delete` | `find ... -delete`, and `find -exec` or `-execdir` running `rm`, when a start path is one the `rm` rule blocks (`/`, `~`, `/home`, `/nix`, `/storage`). `find ./dist -delete` passes like `rm -rf ./dist`. |
| `dynamic-command` | A command whose name is only known when it runs (`"$c"`, `$GIT`, `$(which git)`), when its arguments would be destructive for `rm`, `git`, `chmod`, `chown`, `dd`, `find`, `terraform` or `aws`. `"$EDITOR" notes.md` and `${GO:-go} build` pass. |
| `opaque-shell` | A program's output piped into `sh`, `bash` or `zsh`, or sourced from `<(...)` (`./gen \| sh`, `printf '%s' "$CMD" \| sh`, `source <(./gen)`). A fixed payload is read as itself, a download asks as [Download and run](download-exec.md), and `cat script.sh \| bash` passes like `bash script.sh`. |

```text
ASK: terraform destroy deletes every resource the configuration manages; run terraform plan -destroy first and show the user what goes. Confirm with the user before running it.
```

If a command cannot be parsed, for example it has an unterminated quote, the guard falls back
to the older text match for that command: `echo "don't; rm -rf /` blocks. The spellings that
match misses (`rm -fr /`, `rm  -rf  /`, `git push  -f`) ask instead of blocking, because the
text may not be what a shell would run. This also covers zsh-only syntax such as `${(f)x}`.
A `docker` or
`podman` command is checked by running the rules on the command it runs inside the
container, with bind mounts mapped back to host paths: `docker run -v /:/host ubuntu rm -rf
/host` blocks as `rm -rf /`, while `docker run ubuntu rm -rf /` (the container's own root)
runs.

## Turning it off

You cannot. `disabled_guards` accepts `pii`, `raw-client-data`, `session-url`, `egress`, `git` and
`sweep`, and none of them touches these rules. A test confirms that `rm -rf ~` still blocks
with all six off. To get past a block, run the command yourself in a terminal, or disable
the plugin. See [Options](options.md).
