# Options

Ten plugin options, two files under `~/.config/guardrails/`, and two environment variables
that are not plugin options (`HANDOFF_AUTOCLEAR` and `GUARDRAILS_BIN`). The older
`claude-tools` names still work; see [Older names](#older-names).

Set the plugin options with `/plugin configure guardrails@broodhall`, or from `/config`.

## `disabled_guards`

| | |
| --- | --- |
| Title | Disabled guard families |
| Default | empty (every guard on) |
| Description | Comma-separated families to turn off: pii, raw-client-data, session-url, egress, git, sweep. Every guard is on when this is empty. The core blocks (rm -rf of home or root, git push --force, dd, fork bombs) cannot be turned off. |

The six family names, and what each one controls:

| Family | Controls |
| --- | --- |
| `git` | The asks before `git reset --hard`, `checkout .`, `restore .`, `clean -f`, `branch -D` and `stash`, and before a git command that skips hooks or signing. See [Git](git.md). |
| `egress` | The asks when a command sends a secret, an outside email address or a phone number. See [Egress](egress.md). |
| `pii` | The SQL-output mask, the block on duckdb dot-commands and the block on edits to the PII rules file. See [PII](pii.md). |
| `raw-client-data` | The ask before Python reads a local parquet file or a configured client-data S3 bucket. See [PII](pii.md#raw-client-data-ask). |
| `session-url` | The block on a commit, push or PR that carries a session link. See [Session URLs](session-urls.md). |
| `sweep` | The ask before a sweeping git command (`git add -A`, `commit -a`, `stash`, `checkout .`, `restore .`, `reset --hard`, `clean -f`) takes in a change another live session made. See [Shared checkout](shared-tree.md). |

Core blocks have no switch. [Bash core](bash-core.md), the `git push --force` block,
[Download and run](download-exec.md), [Settings files](settings-files.md) and
[Secret files](secret-files.md) stay on whatever
this option says. [Nudges](nudges.md), [Read limits](read-limits.md) and
[Retry loops](retry-loops.md) have no family name either.

Names are separated by commas, and spaces around a name are ignored. A name that is not one of
the six has no effect. An empty value turns every family on. The launcher falls back to the
`GUARDRAILS_OFF` environment variable, then to empty: every family on. The binary run on
its own reads `CLAUDE_TOOLS_OFF` when `GUARDRAILS_OFF` is unset, and treats both unset as every family on.

## `allowed_email_domains`

| | |
| --- | --- |
| Title | Your email addresses or domains |
| Default | empty |
| Description | Comma-separated addresses or @domains the egress guard treats as yours, such as @example.com. The domain of your global git user.email always counts. |

Outside the plugin the same list is read from `GUARDRAILS_PII_ALLOW`.

- In [Egress](egress.md), a full address matches that address, and `@example.com` matches the
  domain and its subdomains. The domain of `git config --global user.email` always matches.
- In the [PII](pii.md) mask, only full addresses stay readable (and your git address). An
  `@domain` entry has no effect there.

## `observe`

| | |
| --- | --- |
| Title | Observe mode (log only) |
| Default | off |
| Description | Off by default. When on, the optional guards (git, egress, pii, raw-client-data, sweep) allow the call and record what they would have asked or blocked, instead of stopping it. These stay enforced: the core blocks, the settings-file and secret-file guards, config-gate, compact-guard, the large-read block, the session-URL block and egress asks about a secret. Run guardrails guard-report to see the counts. |

Use it to see what the guards would do before you let them stop anything. It is one switch
for all guards, not one per guard.

What it covers is the families that `disabled_guards` can name, except where a mistake
cannot be taken back: `git`, `pii`, `raw-client-data`, `sweep` and the `egress` asks. An ask from one of
them does not ask, and a block from one of them does not block. The call runs.

What it does not cover stays enforced:

- The [core blocks](bash-core.md) and the `git push --force` block.
- [Settings files](settings-files.md) and [Secret files](secret-files.md).
- The [config gate](config-gate.md), which stops edits that would weaken the guards, and the
  compact guard, which blocks a second manual compact.
- The [session-URL](session-urls.md) block. A link that has been pushed cannot be taken back.
- The [egress](egress.md) ask about a secret. A secret that has been sent cannot be taken
  back. The other egress asks (an outside email, a phone number, data the guard cannot read)
  are observed.
- The [Read limits](read-limits.md) block on very large files. The other nudges work as usual.

What you and Claude see:

- Claude gets a note next to the tool result: `Guardrails observe mode: this call ran, but the
  guard would have asked: <the guard's reason> (git-stash)`.
- You see the [receipt](receipt.md) line `would ask 1 (git-stash)` or `would block 1
  (pii-rules)`, and the statusline `guards` count shows the same.
- The guard ledger records `would-ask` or `would-block` with the rule id. Run
  `guardrails guard-report` for the counts across sessions.

Rules are checked in order and the first enforced decision ends the check. A rule after it is
not evaluated, so it is not counted, and a call that an enforced guard stops leaves no
`would-` line. An observed rule never hides a later one. In
`git stash && cat ~/.aws/credentials`, the stash is let through and the secret read is still masked.

Outside the plugin, set `GUARDRAILS_OBSERVE=1`; `true` and `yes` also turn it on. A set
plugin option wins over it. Turn the option off to enforce again.

`guardrails guard-report` ([Run it from a terminal](index.md#run-it-from-a-terminal)) prints one row per rule with how many times it fired, split by
kind. For asks it also shows `ran-after-ask` (the tool ran after the ask, so you approved)
and `not-run` (no follow-up by the session's Stop, or none within 10 minutes for older asks).
A denial and an abandoned prompt look the same, so the report cannot say "denied". A rule
with at least 10 answered asks, 90% or more of them approved, gets a "consider relaxing" hint.
Would-asks from observe mode are never counted as approved.
The report settles old pending asks as it runs, so an approval that comes more than 10 minutes
after its ask is not counted.

## `fail_open`

| | |
| --- | --- |
| Title | Allow tool calls when the guard binary is missing |
| Default | off |
| Description | By default a tool call is blocked, with a message saying how to retry, when the guard binary is not installed (a failed download or checksum). Turn this on to let tool calls through unguarded instead. |

The guards (`pre-tool-use`, `read-dedup`, `large-read-guard` and `config-gate`) fail closed;
the other hooks exit 0 silently, so a missing binary never stops Claude from ending its turn
or compacting. With no binary at the expected
path (never downloaded, a failed download, a checksum mismatch), the launcher exits 2 and
writes this to stderr, which Claude Code shows in place of the tool call (the path and
version differ). The second line reads `The last install failed. Retry now with: ...` when the
background install failed. A missing binary of one version while another version's binary is
installed does not block; see [Updating](../guide/updating.md#between-the-update-and-the-download).

```text
guardrails: BLOCKED: the guard binary guardrails X.Y.Z is not installed (/home/you/.claude/plugins/data/guardrails-broodhall/bin/X.Y.Z/guardrails), so this tool call was not checked.
Installing it in the background now: run the tool call again in a minute. To install by hand: sh "/path/to/plugin/scripts/run.sh" install
Opt out: set the guardrails option fail_open to true (or GUARDRAILS_FAIL_OPEN=1) to allow tool calls without guards.
```

Set `fail_open` to `true` and the launcher exits 0 and prints nothing instead, so every call
runs unguarded and the failed-install notice says so. Outside the plugin the same switch is
the `GUARDRAILS_FAIL_OPEN` environment variable; `true`, `1` and `yes` turn it on. The
session-start install itself never blocks: a failed download only shows a notice and retries
at the next session.

## `client_data_buckets`

| | |
| --- | --- |
| Title | Client-data S3 buckets |
| Default | empty |
| Description | Comma-separated S3 bucket names that hold raw client data; * matches any run of characters, for example *-clientdata. The raw-client-data guard asks before Python reads s3://<bucket>/... Empty means no buckets. |

Outside the plugin the same list is read from `GUARDRAILS_CLIENT_BUCKETS`. See
[PII](pii.md#raw-client-data-ask).

## `masked_copy_dirs`

| | |
| --- | --- |
| Title | Masked-copy directories |
| Default | empty |
| Description | Comma-separated directories that hold masked copies of client data, for example ~/.cache/masked/. The raw-client-data guard does not ask about parquet paths under them. Empty means no exemption. |

Outside the plugin the same list is read from `GUARDRAILS_MASKED_DIRS`. See
[PII](pii.md#raw-client-data-ask).

## `handoff_tokens`

| | |
| --- | --- |
| Title | Handoff threshold (tokens) |
| Default | empty |
| Description | Context size at which the Stop gate asks Claude, once per session, to write a handoff. Leave empty for 80% of CLAUDE_CODE_AUTO_COMPACT_WINDOW (240000 for the default 300000 window). |

Outside the plugin the same number is read from `HANDOFF_TOKENS`. A value that is not a
whole number counts as empty. See [Handoff gate](handoff-gate.md).

## `aws_session_tag`

| | |
| --- | --- |
| Title | Tag AWS calls with the session id |
| Default | off |
| Description | Off by default. When on, each session sets AWS_SDK_UA_APP_ID=claude-<first 8 characters of the session id>, so AWS CLI and SDK calls made from Bash carry it in their User-Agent and CloudTrail records which session made them. |

Opt in to have the `session-env` `SessionStart` hook append
`export AWS_SDK_UA_APP_ID=claude-<id>` to the session's environment file. AWS calls made from
Bash then show `app/claude-<id>` in their User-Agent, and CloudTrail's `userAgent` field names
the session. A session resumed or compacted adds no second line; a new session id adds one.

The hook never blocks and prints nothing. It writes nothing for a missing or unsafe session id.
Outside the plugin, set `GUARDRAILS_AWS_SESSION_TAG=1`; a set plugin option wins over it.

## `rtk`

| | |
| --- | --- |
| Title | Rewrite commands with RTK |
| Default | off |
| Description | Off by default. When on, the pre-tool-use hook runs `rtk hook claude` after its own checks and applies RTK's token-saving command rewrite. Needs RTK installed. Do not also install RTK's own Claude Code hook. |

RTK rewrites shell commands such as `git status` into
`rtk git status`, whose output is shorter. With this option on, the guardrails hook calls
`rtk hook claude` itself.

**Do not also install RTK's own Claude Code hook.** Claude Code runs the hooks for one tool
call in parallel and keeps the command from whichever finishes last. A second RTK hook
could overwrite the PII mask or the secret-file mask with a plain RTK rewrite. With the
option on, one hook decides the order:

1. Every guard reads the command as typed. A block or an ask ends the call, and rtk never
   sees it.
2. A secret-file mask or a PII wrap, if one applies, becomes the new command, and rtk is
   not run for it.
3. Otherwise rtk may rewrite the command. A guard's note still reaches Claude next to
   rtk's rewrite.

A command that points git at another place with `-C`, `--git-dir` or `--work-tree` is never
rewritten. `rtk git -C dir status` would hide `dir` from any guard that reads the git
target, such as a worktree-isolation check, so those commands run as typed.

If the option is on and rtk cannot be found, the guards run as usual, the command is not
rewritten, and the hook prints one note on stderr per session. It never blocks. A rtk
that fails, or runs for more than 5 seconds, is treated the same way, without a note.
rtk's own deny (exit code 2) is passed through as a block.

Outside the plugin, set `GUARDRAILS_RTK=1`.

## `rtk_command`

| | |
| --- | --- |
| Title | RTK command or path |
| Default | empty |
| Description | The rtk executable to run when the rtk option is on. Empty means `rtk` from PATH. |

Use this when rtk is not on the PATH that Claude Code's hooks see, for example
`/opt/tools/bin/rtk`. Outside the plugin it is read from `GUARDRAILS_RTK_CMD`.

## `HANDOFF_AUTOCLEAR`

Set to `0` in the environment Claude Code starts in to stop the [handoff
chain](handoff-gate.md#the-chain) short of the clear. The handoff is still written, but
`autoclear` arms nothing and logs `no clear: HANDOFF_AUTOCLEAR=0`, and the gate's message tells
Claude that you will run `/clear` yourself. Any other value, or none, leaves the clear on.
It is not a plugin option.

## `~/.config/guardrails/secret-paths`

Extra files for [Secret files](secret-files.md) to protect, one path or glob per line. The
list adds to the built-in ones; it does not replace them. The file itself is protected too.

```text
# my deploy credentials
~/.config/myapp/credentials.toml
~/work/*/.env.production
```

- `~` is expanded to your home directory. Blank lines are skipped, and so are lines that start
  with `#`. A `#` after text on a line is part of the path.
- A line that is not an absolute path after `~` is expanded names no file and is ignored.
- If `XDG_CONFIG_HOME` is set to an absolute path, the file is read from
  `$XDG_CONFIG_HOME/guardrails/secret-paths` instead.
- The file is read on each tool call, so a new line applies at once.

## `~/.config/guardrails/pii-columns.json`

The column rules for the [PII](pii.md) mask. It is used only when the `pii` family is on. The
location follows the same `XDG_CONFIG_HOME` rule as above. Claude is blocked from writing it.

The file is JSON with two keys that matter. Other keys, such as `notes`, are ignored.

```json
{
  "rules": [
    {
      "name": "email",
      "category": "email",
      "tier": 1,
      "pattern": "(?:[a-z0-9_]*_)?e_?mail(?:_?addr(?:ess)?)?",
      "exclude": ".*_(?:enabled|flag|verified|sent|count|type|status|template|subject|opt_?in)"
    }
  ],
  "explicit_columns": ["driver_id"]
}
```

- `name` identifies the rule. `category` is the tag in the masked cell (`[email:3229ba]`) and
  must be one of the categories the built-in rules use: `address`, `address_coarse`,
  `credentials`, `demographic`, `dob_age`, `email`, `financial`, `financial_policy_claim`,
  `free_text`, `gov_id`, `health_info`, `ip_device`, `json_blob`, `other_identifier`,
  `person_name` and `phone_fax`. A file with any other category is rejected.
- `pattern` is a regular expression matched against the whole column name after it is
  lowercased and converted from camelCase to snake_case (a trailing number is also tried
  without, so `email2` meets the `email` rule). It is anchored: it must match the whole name.
  `exclude`, if present, is a second pattern that stops a match.
- `tier` decides whether a rule is used. Tier 1 rules always are. Of the tier 2 rules, only
  those named `policy_claim_number`, `vin` and `demographic` are. All other rules are skipped.
- `explicit_columns` lists column names to mask as `[key:tag]`, by exact name.

Email and phone shapes in any value are masked whatever the file says.

Your file replaces the built-in rules. It does not add to them. If it is missing, unreadable,
not valid JSON, has a bad pattern or an unknown category, or has no active rule, the built-in
rules are used and nothing is reported at mask time. To start from the built-in rules and
edit them, run `guardrails pii-rules init` ([Run it from a terminal](index.md#run-it-from-a-terminal)), which writes the file and will not overwrite an
existing one. To check a file, run `guardrails pii-rules check [path]`:

```text
ok /home/you/.config/guardrails/pii-columns.json: 28 rules active (33 in file), 1 explicit columns
```

```text
invalid bad.json: pii-columns.json: rule 0 ("x"): unknown category "bogus"
```

## `GUARDRAILS_BIN`

The hooks run one binary. By default that is the copy the plugin downloads for its version, to
`bin/<version>/guardrails` under the plugin's data directory. Set `GUARDRAILS_BIN` to the
path of your own build to run that instead, for example one you built from this repository.

The download is skipped only when the path is executable. If the path is not an executable
file, the session-start step downloads the release copy anyway, but the hooks still look
at your path and find nothing to run, so every call is blocked (or, with
[`fail_open`](#fail_open), let through unguarded). Check the path before you rely on it.

## Older names

The binary was called `claude-tools` before it was renamed `guardrails`. Your existing setup
keeps working:

- **Files.** Each file is looked for under `guardrails/` first, then under `claude-tools/`,
  and a new one is created under `guardrails/`. So an existing
  `~/.config/claude-tools/pii-columns.json`, `~/.config/claude-tools/secret-paths` or
  `~/.local/state/claude-tools/pii-key` stays in use, and your PII tags do not change. To move
  one, move the file; a file under `guardrails/` wins when both exist. `secret-paths` is read
  from both directories.
- **Environment variables.** Each `GUARDRAILS_<NAME>` falls back to `CLAUDE_TOOLS_<NAME>`
  when it is unset. A `GUARDRAILS_<NAME>` that is set, even to an empty value, wins.
- **Guards.** The [PII](pii.md) rules-file block and the [Secret files](secret-files.md)
  protection cover both directories, and the [config gate](config-gate.md) treats a change to
  either kind of variable as weakening a guard.
