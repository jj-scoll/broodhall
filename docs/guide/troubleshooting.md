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
