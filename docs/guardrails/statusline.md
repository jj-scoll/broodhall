# Status line

`guardrails statusline` prints a two-line status line from the JSON Claude Code pipes to a
`statusLine` command: the working directory, git branch, model, context use, prompt-cache
state, rate limits, and a count of what the guards did this session. It reads the
transcript's tail for cache data and never fails: a payload it cannot read still prints a
line.

## Turn it on

A plugin cannot set `statusLine`. Claude Code reads only the `agent` and
`subagentStatusLine` keys from a plugin's own settings, so the status line is wired by hand
in your `settings.json`. Point `command` at the binary the launcher installed, which lives
at `bin/<version>/guardrails` under the plugin's data directory (`CLAUDE_PLUGIN_DATA`).
Run `guardrails doctor` from a shell where that variable is set, or read the `ok binary`
line of its report, to see the exact path.

```json
{
  "statusLine": {
    "type": "command",
    "command": "/path/to/guardrails statusline",
    "refreshInterval": 30
  }
}
```

The path changes with the plugin version, because each version installs into its own
directory. To keep working across updates, let the command pick the newest installed
version each time it runs:

```json
{
  "statusLine": {
    "type": "command",
    "command": "sh -c 'c=${CLAUDE_CONFIG_DIR:-$HOME/.claude}; b=$(ls -d \"$c\"/plugins/data/guardrails-broodhall/bin/*/guardrails 2>/dev/null | sort -V | tail -1); [ -z \"$b\" ] || exec \"$b\" statusline'",
    "refreshInterval": 30
  }
}
```

A command copied from an older version of this page looks for `bin/*/claude-tools`, so it keeps
running the last binary with that name. Replace it with the one above.

This command needs no `HANDOFF_STATE_DIR`: the binary finds the handoff state in the plugin
data directory it lives in (see [handoff-gate.md](handoff-gate.md#state)).

- The data directory is named `guardrails-<marketplace>`; change `guardrails-broodhall` if
  you installed from a marketplace with another name.
- `CLAUDE_CONFIG_DIR` is honored, so one shared `settings.json` works for several accounts,
  each finding the binary under its own config directory.
- With no binary installed (before the first session downloads it), the command prints
  nothing and exits 0.
- Add flags after `statusline`, for example
  `statusline -account-default personal -account .claude-work=work`.
- Tested on Linux with GNU `sort`. macOS `sort -V` is not verified.

Or copy the binary to a stable place such as `~/.claude/bin/` and point `command` there. A
copied binary does not update itself.

!!! note "Not verified"
    Whether `${CLAUDE_PLUGIN_ROOT}` or `${CLAUDE_PLUGIN_DATA}` expand inside a user
    `settings.json` `statusLine.command` is not documented, and plugin `userConfig` values
    do not reach a `statusLine` command. Use an absolute path, and put options in the
    command as flags.

## Options

Options are flags in the `command`. With none, you get the general line below. Nothing
account- or machine-specific is on by default.

| Flag | Default | Effect |
| --- | --- | --- |
| `-segments a,b,c` | every segment | Show only the named segments. Names it does not know are ignored. |
| `-ctx-warn N` | 70% of the window | Context tokens at which `ctx` turns amber. |
| `-ctx-limit N` | 90% of the window | Context tokens at which `ctx` turns red. Also shown as the denominator, as in `ctx 200k/260k`. |
| `-keepalive-marker TEXT` | none | A prompt containing `TEXT` counts as a cache keep-alive ping. Turns on the `keepalive` segment. |
| `-keepalive-max N` | `4` | The N in the `♻n/N` ping counter. |
| `-account-default LABEL` | none | Badge on line 1 for any config directory not named by `-account`. |
| `-account DIR=LABEL` | none | Badge `LABEL` when `CLAUDE_CONFIG_DIR` ends in a directory named `DIR`, such as `.claude-second=second`. Repeatable. |
| `-usage-cache FILE` | none | Write the 5-hour and 7-day usage to `FILE` as JSON, for a desktop widget. |

The `ctx` thresholds scale with the context window the payload reports. Set `-ctx-warn` and
`-ctx-limit` in tokens to pin them, for example `-ctx-warn 180000 -ctx-limit 260000`.

The account badge is off until you pass `-account-default` or `-account`. With no badge and
nothing else for line 1, the line is not printed, so you get one line.

An unknown flag does not stop the line: the options before it apply, the rest are defaults,
and the error goes to stderr.

### Segments

Line 1 (identity), shown only when the payload has the field:

`account`, `session` (name), `pr`, `worktree`, `effort` (hidden at `medium`), `style`
(hidden at `default`).

Line 2:

| Segment | Shows |
| --- | --- |
| `host`, `user` | Host and user name. |
| `dir`, `branch` | Working directory and git branch. |
| `time` | Clock time. |
| `model` | Model name. |
| `elapsed` | Session minutes. |
| `ctx` | Context tokens against the thresholds. |
| `cache` | Prompt-cache hit percentage, green when warm. |
| `ttl` | Time left on the prompt cache, or `cold`, and what a cold start would recache. |
| `keepalive` | Keep-alive pings since your last real prompt. Needs `-keepalive-marker`. |
| `totals` | Session cache reads and writes from the transcript. |
| `reqs` | Cache requests and misses. |
| `five`, `reset` | 5-hour rate-limit use and the time to its reset. |
| `week` | 7-day rate-limit use. |
| `handoff` | Progress of the [auto-handoff](handoff-gate.md) for this session, only while it applies. See below. |
| `rc` | `⇄ rc` when the transcript shows [Remote Control](https://code.claude.com/docs/en/remote-control) connected for this session. Nothing otherwise. A best guess; see below. |
| `guards` | What the guards masked, asked about, blocked or noted this session. |
| `drift` | A warning when the last 60 tool calls repeat one read, command or edit. |

On a narrow terminal (`$COLUMNS`) the lowest-value segments drop first. `dir` and `model`
never drop.

### The handoff segment

Shown only while the [handoff chain](handoff-gate.md#the-chain) is doing something for the
session in the payload (`session_id`), and briefly after. With no state it prints nothing. It
stats a few small files in the handoff state directory and never reads the transcript.
Turn it off by leaving `handoff` out of `-segments`.

| Text | Colour | Means |
| --- | --- | --- |
| `handoff: next-steps` | cyan | The gate blocked the stop; Claude is writing the next-steps file. Shown for up to 10 minutes. |
| `handoff: writing 23s` | cyan | The background writer is running, with the time since it started. |
| `handoff: clear armed` | amber | The handoff is written; context-guard has not yet taken the clear. |
| `handoff: clearing` | cyan | context-guard claimed the clear and is running it. |
| `handoff: cleared → HANDOFF.md` | green | The clear ran. Shown for 2 minutes. |
| `handoff: failed (see writer.log)` | red | The writer failed. Shown for 2 minutes; `writer.log` has the reason. |
| `handoff: written → HANDOFF.md, run /clear` | amber | `HANDOFF_AUTOCLEAR=0`: nothing clears itself. Shown for 10 minutes. |

A `writing` marker older than 15 minutes is taken for a dead writer and ignored. A new gate
mark newer than the last writer start shows `next-steps` in place of an earlier outcome.

### The rc segment

Claude Code does not pass Remote Control state to a `statusLine` command, so the segment
reads the session transcript. Claude Code writes a `bridge-session` line into it about every
20 to 40 entries. That line is not a heartbeat: it keeps coming after Remote Control
disconnects. So the segment shows `⇄ rc` only when both hold:

- a `bridge-session` line is among the last 100 lines, and
- the newest connection notice in the last 1 MiB or so is not "Remote Control disconnected" or
  "Remote Control not started here". An "is active" notice, or no notice at all, passes.

It reads at most the last 4 MiB of the file. A missing or unreadable transcript shows
nothing. Turn it off by leaving `rc` out of `-segments`.

It can be wrong in these ways:

- Turning Remote Control off yourself may leave no notice. The segment then stays lit. This
  case is unverified.
- A resumed or forked session inherits the `bridge-session` line. It shows `⇄ rc` until a
  notice says otherwise, even if this terminal never connected.
- A disconnect notice more than about 1 MiB back is not seen. In a sample of real sessions
  about one disconnected period in five ran that long.
- Some "account changed" disconnects may reconnect without a notice. The segment then stays
  dark until the next "is active" notice. This is inferred, not confirmed.

## Examples

Only the essentials:

```json
"command": "/path/to/guardrails statusline -segments dir,branch,model,ctx,five,week"
```

Two accounts, with a keep-alive counter and a widget file:

```json
"command": "/path/to/guardrails statusline -account-default main -account .claude-second=second -keepalive-marker 'keep-alive ping' -usage-cache /home/me/.cache/widget/usage.json"
```

## The usage cache

With `-usage-cache FILE`, each render merges the 5-hour and 7-day figures into `FILE` as one
JSON object (`ctx`, `five`, `five_reset`, `week`, `week_reset`). Many sessions share the
file: within a window the highest value wins, and an older window never replaces a newer
one. The write goes to a temporary file and is renamed into place, under a lock on
`FILE.lock` that gives up after a second. Use a different file for each account.
