# guardrails

Guards for Claude Code tool calls, as a plugin: it blocks destructive shell commands, asks
before destructive git, settings edits and secret-file reads, masks secrets in output, and
stops repeated or oversized Reads and retry loops. The hooks are one static Go binary.

## Install

```sh
claude plugin marketplace add jj-scoll/guardrails
claude plugin install guardrails@jj-scoll
```

Or in a session: `/plugin marketplace add jj-scoll/guardrails`, then `/plugin install
guardrails@jj-scoll`. Start a new session afterwards. On its first SessionStart the plugin
downloads a ~4 MB static binary for its version from this repo's GitHub release, checks its
sha256, and keeps it in `~/.claude/plugins/data/`. Linux and macOS, amd64 and arm64; it needs
`curl` and `tar`.

If the download fails, the guards stay off for that session (you see a one-line notice) and
tool calls run normally. They never block on a missing binary. The next session retries.

Once installed, the guards fail closed: if the binary crashes, is killed or hangs, the tool
call it was checking is blocked rather than let through unchecked. If a broken binary blocks
every tool call, run `claude plugin disable guardrails@jj-scoll` in a terminal and start a
new session.

## What it does

| Hook | Behavior |
| --- | --- |
| Bash, core | **Blocks** `rm -rf` of `/`, `~`, `$HOME` or system dirs, `git push --force` (`--force-with-lease` passes), `dd if=`, redirects into `/etc/`, `chmod -R 777 /`, fork bombs, privileged containers. Text inside echo, commit messages, grep patterns or heredocs does not count. |
| Bash, git | **Asks** before `reset --hard`, `branch -D`, `stash` drop/clear, `checkout`/`restore .`, `clean -f`. |
| Settings | **Asks** before the model writes a `.claude/settings.json` or `settings.local.json`. |
| Secret files | SSH keys, AWS, Claude, `gh` and Docker credentials, `.netrc`, `.pgpass`, plus the paths or globs in `~/.config/claude-tools/secret-paths`: Bash reads are rewritten to show the file with values masked, Read is blocked in favor of that, Edit/Write ask. |
| Egress | **Asks** when `gh`, `curl`, `wget`, `aws s3` or an MCP send carries a secret, or an email address or phone number that isn't yours. |
| Nudges | `terraform`/`terragrunt apply` with no recent `plan`, `docker build`: a note to Claude, no block. |
| Read | Notes a re-read of an unchanged file; denies whole-file reads of code over ~10k tokens and points at offset/limit. |
| Retry loops | After the 3rd failure of the same Bash command, reminds Claude to stop and report. |

## Options

`/plugin configure guardrails@jj-scoll`, or the guardrails rows in `/config`:

- **disabled_guards** (default `pii,raw-client-data,session-url`): comma-separated families
  to turn off. `pii` masks PII columns in `psql`/`duckdb` output by column name (rules in
  `cmd/claude-tools/pii-columns.json`; add your own in `~/.config/claude-tools/pii-columns.json`);
  `raw-client-data` asks before Python reads local parquet files, whose output is not masked;
  `session-url` blocks Claude session links in commits and PRs; `egress` and `git` are the
  rows above. Clear the field to run everything. The core blocks have no switch.
- **allowed_email_domains**: addresses or `@domains` the egress guard treats as yours, such
  as `@example.com`. The domain of your global `git config user.email` always counts.

To test a local build in place of the download, set `CLAUDE_TOOLS_BIN` to its path in your
settings `env`.

## Build

```sh
CGO_ENABLED=0 go build -o claude-tools ./cmd/claude-tools
CGO_ENABLED=0 go test ./...
claude plugin validate .
```

## Release

Bump `version` in `plugins/guardrails/.claude-plugin/plugin.json`, commit, and push tag
`v<version>`. `.github/workflows/release.yml` checks the tag matches, runs the tests, and
publishes `claude-tools_<os>_<arch>.tar.gz` plus `checksums.txt`. Installed plugins pick up
the new version on update and fetch its binary at the next session start.

## License

MIT
