# First session

After you install, start a new session. At session start the plugin checks for the binary
for its version and downloads it if it is missing, as described on the
[Install](install.md) page. A failed download shows a one-line notice that starts
`guardrails: could not install claude-tools`. Until the binary installs, tool calls are blocked
with a message that says how to retry; the notice and the [`fail_open`](../guardrails/options.md#fail_open)
option cover the alternative. The next session start tries again.

## Defaults

Every guard family is on by default. The core blocks (recursive delete of `/`, `~`, `$HOME`, and a few
top-level directories such as `/home`, `git push --force`, `dd if=`, fork bombs) cannot be turned off,
and `disabled_guards` turns the optional families off. See
[Options](../guardrails/options.md).

## Try it in observe mode first

If you do not want a guard to stop you yet, turn on the
[`observe`](../guardrails/options.md#observe) option with `/plugin configure guardrails@broodhall`.
The git, PII, raw-client-data and most egress guards then let the call run and record
what they would have done. You see a `would ask` or `would block` count in the receipt line.

After a few days, run `claude-tools guard-report` to see which rules fired and how often. Then
turn `observe` off. Some guards stay enforced in observe mode too: the core blocks, the
settings-file and secret-file guards, config-gate, compact-guard, the large-read block, the
session-URL block and egress asks about a secret. See [options](../guardrails/options.md).

## See it work

In a throwaway repository, ask Claude to run `git reset --hard`. Claude Code stops and shows
you the guard's reason before anything runs. That is the ASK response described on
[Why](why.md). Nothing is discarded unless you approve.

### memory

memory downloads in the background during the first session and starts from the second. In
the second session, check that the daemon runs and serves its page:

```sh
pgrep -f 'memory serve'
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:9495/
```

The first prints a process id, the second `200`. Open `http://127.0.0.1:9495/` in a browser
to see the notes as they arrive. If either fails, read `memory.log` in the plugin data
directory, `~/.claude/plugins/data/memory-broodhall/memory.log`, and see
[Troubleshooting](troubleshooting.md#memory-does-not-start).

After a few days, the memory `cost` report shows token cost per day and session. `memory` is
not on your `PATH`; the command to run it is under
[Token cost and cache misses](../memory/index.md#token-cost-and-cache-misses).

### context-guard

The warning normally starts at 180000 tokens. To see it sooner, set `warn_tokens` to `1000`
with `/plugin configure context-guard@broodhall` and start a new session. Once the session
has more than 1000 tokens of context, which is after the first reply, each prompt shows a line
such as `context 25k of 260k limit`. Set `warn_tokens` back to `180000` afterwards.

### rtk

A healthy rtk plugin shows nothing at session start; it only speaks when rtk is missing or is
not the fork (see [RTK](../rtk/index.md#what-it-does)). To see the rewrite, turn on the
guardrails [`rtk` option](../guardrails/options.md#rtk), start a new session and ask Claude to
run `git status`. Then, in a terminal, run:

```sh
rtk gain --history
```

`rtk git status` is listed under "Recent Commands".

## Check the install

`claude plugin list` shows which plugins are installed and enabled. If a guard does not
fire when you expect it to, see [Troubleshooting](troubleshooting.md).
