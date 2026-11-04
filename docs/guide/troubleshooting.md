# Troubleshooting

## Every tool call is blocked

Once installed, the guards fail closed: if the binary fails while checking a call, that call
is blocked. If a broken binary blocks everything, turn the plugin off from a terminal and
start a new session:

```
claude plugin disable guardrails@broodhall
```

Use `claude plugin enable guardrails@broodhall` to turn it back on.

## "guardrails: could not install claude-tools"

The launcher prints this at session start, with the plugin version and a reason in
parentheses, and leaves the guards off for that session. Tool calls run normally and the
next session start retries. The reasons:

| Reason | Meaning |
| --- | --- |
| `unsupported CPU <name>` | The CPU is neither amd64 nor arm64. |
| `cannot write <dir>` | The plugin data directory is not writable. |
| `download of <file> failed` | `curl` could not fetch the tarball or `checksums.txt` within its 60 and 30 second limits, or the platform has no release asset. |
| `checksum mismatch` | The tarball's sha256 did not match `checksums.txt`, or `checksums.txt` has no line for the tarball. It was not installed. |
| `unpack failed` | `tar` could not extract the binary, or it could not be moved into place. |

## A doctor message at session start

The launcher runs `claude-tools doctor -hook` after the install step and shows its message
only when something is wrong. [Doctor](../guardrails/doctor.md) lists each check and what
passes it. It never blocks a session.

## Guards seem to be off

- The binary may be missing. With no binary, the launcher allows every call. Start a new
  session and look for the notice above.
- A guard family may be disabled. `pii`, `raw-client-data` and `session-url` are off by
  default. Check the plugin's options with `claude plugin configure guardrails@broodhall`,
  and see [Options](../guardrails/options.md).
- `CLAUDE_TOOLS_BIN` may point at a binary that does not exist or is not executable. The
  launcher uses that path in place of the downloaded binary, and with no executable there it
  allows every call.

If you installed from the old `jj-scoll` marketplace, see [Moving from the old jj-scoll marketplace](install.md#moving-from-the-old-jj-scoll-marketplace).

## memory does not start

memory fails open, so a problem never blocks a session; it just leaves memory off. Look
first at `memory.log` in the plugin data directory, where the launcher writes its notes.

| Log line | Meaning |
| --- | --- |
| `unsupported OS` or `unsupported CPU` | memory runs on Linux and macOS, on amd64 and arm64. |
| `curl not found` | Install `curl`. |
| `xz not found (install xz-utils)` | On Linux the archive is `.tar.xz` and needs the `xz` program. |
| `download of <file> failed` | `curl` could not fetch the archive (10 minute limit) or `checksums.txt` (30 seconds). |
| `checksum mismatch or missing` | The archive did not match `checksums.txt`. It was not installed and is never run. |
| `unpack of <file> failed` | `tar` could not unpack it, or it lacked the program or an extension. |
| `install of <version> failed within the last hour; not retrying` | A failed install leaves a stamp, so an offline machine does not wait on the network at every start. Delete `bin/.fail-<version>` in the plugin data directory to retry now. |

Other causes:

- **First session.** The download runs in the background, so memory starts from the second
  session. Wait for the download and start a new session.
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
