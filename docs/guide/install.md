# Install

```
claude plugin marketplace add jj-scoll/broodhall
claude plugin install guardrails@broodhall
```

Or inside a session, use the `/plugin` command and add the same marketplace and plugin.
Start a new session afterwards.

## Requirements

- Linux or macOS, on amd64 or arm64. Those are the four binaries the release publishes.
- `curl` and `tar` on your `PATH`. The launcher uses them to fetch and unpack the binary, and
  `sha256sum` or `shasum` to check it.

## What gets downloaded

On its first SessionStart, and again at the first SessionStart after each plugin update, the
plugin downloads the static binary for its own version from this repo's GitHub release. The
download is a `claude-tools_<os>_<arch>.tar.gz` of about 1.7 MB that unpacks to a binary of
about 4 MB. It is checked against the release's `checksums.txt` by sha256, then kept in the
plugin's data directory under `bin/<version>/`, so a version is downloaded once.

If the download fails, the guards stay off for that session and you see a one-line notice.
Tool calls run normally. The next session start tries again.

## Test a local build

Set `CLAUDE_TOOLS_BIN`, in the environment Claude Code runs in, to the path of a binary you
built. The launcher then uses that binary and skips the download.

```
CGO_ENABLED=0 go build -o claude-tools ./cmd/claude-tools
```

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
