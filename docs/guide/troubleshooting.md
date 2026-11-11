# Troubleshooting

## Every tool call is blocked

Once installed, the guards fail closed: if the binary fails while checking a call, that call
is blocked, and so is every call while the binary is not installed. If that blocks
everything, either turn on the `fail_open` option (it only affects a missing binary), or turn
the plugin off from a terminal and start a new session:

```
claude plugin disable guardrails@broodhall
```

Use `claude plugin enable guardrails@broodhall` to turn it back on.

## "guardrails: could not install guardrails"

The launcher prints this at session start, with the plugin version and a reason in
parentheses (older releases say `could not install claude-tools`). The session still starts,
but until the binary installs every tool call is blocked with a message that says how to retry
and opt out; with `fail_open` set they run unguarded instead. The next session start retries, and so does
`sh "<plugin dir>/scripts/run.sh" install`. The reasons:

| Reason | Meaning |
| --- | --- |
| `unsupported CPU <name>` | The CPU is neither amd64 nor arm64. |
| `curl not found` | Install `curl`. |
| `cannot write <dir>` | The plugin data directory is not writable. |
| `download of <file> failed` | `curl` could not fetch the tarball within 60 seconds (or, for a plugin with no `checksums.txt` of its own, the release's within 30), or the platform has no release asset. |
| `the plugin's checksums.txt is not for version <version>` | The `checksums.txt` in the plugin directory names another version than `plugin.json`. Nothing was downloaded. Update the plugin; if it persists, the release is broken: report it. |
| `no checksum for <file> in the plugin's checksums.txt` | The `checksums.txt` in the plugin directory has no line for the tarball. It was not installed. |
| `checksum mismatch against the plugin's checksums.txt` | The tarball in the release is not the one the release commit recorded. It was not installed. Do not work around it: report it. |
| `no checksum for <file> in the release's checksums.txt`, `checksum mismatch against the release's checksums.txt` | The same two, for a plugin with no `checksums.txt` of its own, which checks against the release's copy. See [How a download is checked](install.md#how-a-download-is-checked). |
| `unpack failed` | `tar` could not extract the binary, or it could not be moved into place. |
| `another install is running` | Another session was installing and did not finish within 90 seconds. Start a new session once it is done. |

## A doctor message at session start

The launcher runs `guardrails doctor -hook` after the install step and shows its message
only when something is wrong. [Doctor](../guardrails/doctor.md) lists each check and what
passes it. It never blocks a session.

## Guards seem to be off

- The binary may be missing and `fail_open` on. With no binary the launcher allows every call
  only when `fail_open` (or `GUARDRAILS_FAIL_OPEN`) is set. Start a new session and look
  for the notice above.
- A guard family may be disabled. Every family is on by
  default, so check `disabled_guards`. Check the plugin's options with `claude plugin configure guardrails@broodhall`,
  and see [Options](../guardrails/options.md).
- `GUARDRAILS_BIN` may point at a binary that does not exist or is not executable. The
  launcher uses that path in place of the downloaded binary, and with no executable there it
  blocks every call (or allows every call under `fail_open`).

If you installed from the old `jj-scoll` marketplace, see [Moving from the old jj-scoll marketplace](install.md#moving-from-the-old-jj-scoll-marketplace).

## memory does not start

memory fails open, so a problem never blocks a session; it just leaves memory off. Look
first at `memory.log` in the plugin data directory, where the launcher writes its notes.

| Log line | Meaning |
| --- | --- |
| `unsupported OS` or `unsupported CPU` | memory runs on Linux and macOS, on amd64 and arm64. |
| `curl not found` | Install `curl`. |
| `xz not found (install xz-utils)` | On Linux the archive is `.tar.xz` and needs the `xz` program. |
| `download of <file> failed` | `curl` could not fetch the archive (10 minute limit) or, for a plugin with no `checksums.txt` of its own, the release's (30 seconds). |
| `the plugin's checksums.txt is not for version <version>` | The `checksums.txt` in the plugin directory names another version than `plugin.json`. Nothing was downloaded. |
| `the plugin's checksums.txt has no entry for <file>` | The `checksums.txt` in the plugin directory has no line for the archive. It was not installed. |
| `checksum mismatch for <file> against the plugin's checksums.txt` | The archive in the release is not the one the release commit recorded. It was not installed and is never run. Report it. |
| `the release's checksums.txt has no entry for <file>`, `checksum mismatch for <file> against the release's checksums.txt` | The same two, for a plugin with no `checksums.txt` of its own. See [How a download is checked](install.md#how-a-download-is-checked). |
| `unpack of <file> failed` | `tar` could not unpack it, or it lacked the program or an extension. |
| `<file> has no ext/<name>.duckdb_extension` | The archive lacks a DuckDB extension memory needs. It was not installed. |
| `cannot write <dir>` | The plugin data directory is not writable. |
| `install into <dir> failed` | The unpacked files could not be moved into `bin/<version>/`. Check the directory's permissions and free space. |
| `MEMORY_BIN=<path> is not executable` | `MEMORY_BIN` is set but points at no runnable file. Fix or unset it. |
| `install of <version> failed within the last hour; not retrying` | A failed install leaves a stamp, so an offline machine does not wait on the network at every start. Delete `bin/.fail-<version>` in the plugin data directory to retry now. |
| `memory serve: <socket>: already in use by a running daemon` | Normal. Every session start launches the daemon, and a second one stops because the first holds the socket. |

To retry the install in the foreground, delete `bin/.fail-<version>` and run
`sh "<plugin dir>/scripts/run.sh" install </dev/null`. Only `curl`'s own error shows in the
terminal. The launcher's reason (one of the messages above) is added to `memory.log`.

Other causes:

- **First session.** The download runs in the background, and the daemon starts when it
  lands. Until then nothing is recorded, and the context block at session start begins with
  the next session. Wait for the download and start a new session.
- **Alpine or another musl Linux.** The program needs glibc 2.35 or newer. It will not run
  on musl.
- **No notes appear.** The daemon writes notes by running `claude -p`. If `claude` is not on
  the `PATH` of the daemon or is signed out, the tool calls stay queued and nothing is lost.
  Fix the sign-in and they are processed. Check `memory.log` in the state directory
  (`~/.local/share/memory/`).
- **UI or Quack unavailable.** Ports `9495` and `9494` are fixed. If another program has one,
  the daemon logs that it is not serving it and carries on. See
  [UI and Quack](../memory/ui-and-quack.md).
- **Two daemons.** Only one can run per state directory. A second exits at once.
- **A changed setting has no effect.** The daemon reads its settings when it starts. Stop it
  with `pkill -f 'memory serve'` and start a new session.
