# Install

```
claude plugin marketplace add jj-scoll/broodhall
claude plugin install guardrails@broodhall
```

For the [memory](../memory/index.md) plugin, add this line:

```
claude plugin install memory@broodhall
```

The two plugins are independent. Install either or both.

Or inside a session, use the `/plugin` command and add the same marketplace and plugin.
Start a new session afterwards.

## Requirements

- Linux or macOS, on amd64 or arm64. Those are the four binaries the release publishes.
- `curl` and `tar` on your `PATH`. The launcher uses them to fetch and unpack the binary, and
  `sha256sum` or `shasum` to check it.

## Requirements for memory

- Linux or macOS, on amd64 or arm64.
- On Linux, a glibc system (Debian, Ubuntu, Fedora and similar), glibc 2.35 or newer. musl
  systems such as Alpine are not supported, because the program is not static.
- On Linux, `xz` on your `PATH` (the `xz-utils` package on Debian and Ubuntu). macOS needs
  nothing extra.
- `curl`, `tar`, and `sha256sum` or `shasum`, as for guardrails.
- The `claude` command on your `PATH`, signed in. memory runs it to write its notes. Read
  [Privacy and redaction](../memory/privacy.md) before you install.

## What gets downloaded

On its first SessionStart, and again at the first SessionStart after each plugin update, the
plugin downloads the static binary for its own version from this repo's GitHub release. The
download is a `claude-tools_<os>_<arch>.tar.gz` of about 1.7 MB that unpacks to a binary of
about 4 MB. It is checked against the release's `checksums.txt` by sha256, then kept in the
plugin's data directory under `bin/<version>/`, so a version is downloaded once.

If the download fails, the guards stay off for that session and you see a one-line notice.
Tool calls run normally. The next session start tries again.

### memory

memory downloads differently, because it is large and must not hold up a session. At the
first SessionStart after install, and again after each plugin update, the launcher starts the
download in the background and the session goes on without memory. The download is a
`memory_<os>_<arch>.tar.xz` of about 33 MB. It holds the program and three signed DuckDB
extensions, so nothing more is fetched later. It is checked against the release's
`checksums.txt` by sha256, then kept in the plugin's data directory under `bin/<version>/`.

So memory starts from the next session, not the one where you installed it. When the
download finishes, the launcher starts the daemon. If the download fails, memory stays off
and says why in `memory.log` in the plugin data directory. It does not retry for an hour.
See [Troubleshooting](troubleshooting.md#memory-does-not-start).

## Test a local build

Set `CLAUDE_TOOLS_BIN`, in the environment Claude Code runs in, to the path of a binary you
built. The launcher then uses that binary and skips the download.

```
CGO_ENABLED=0 go build -o claude-tools ./cmd/claude-tools
```

For memory, set `MEMORY_BIN` instead. The program needs cgo and the DuckDB extensions; see
[Build](../reference/build.md).

## Moving from the old jj-scoll marketplace

Earlier versions were installed from a marketplace named `jj-scoll`. That install keeps
working, but it stays on 0.1.2: updating it does not move it to `broodhall`. To move over,
remove the old marketplace, then add the new one:

```
claude plugin marketplace remove jj-scoll
claude plugin marketplace add jj-scoll/broodhall
claude plugin install guardrails@broodhall
```

Removing the `jj-scoll` marketplace also uninstalls the guardrails plugin installed from it
and deletes that plugin's saved options and data, so set any options you changed again with
`/plugin configure guardrails@broodhall`. The plugin keeps its name, `guardrails`; only the
marketplace it comes from changes. You can do the same from the `/plugin` interface.

The first session after the move downloads the binary again, and the per-session guard
state starts fresh. A cached copy of the old plugin may be left under `plugins/cache/jj-scoll`
in your Claude config directory; it is safe to delete.
