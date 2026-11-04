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

## See it work

In a throwaway repository, ask Claude to run `git reset --hard`. Claude Code stops and shows
you the guard's reason before anything runs. That is the ASK response described on
[Why](why.md). Nothing is discarded unless you approve.

## Check the install

`claude plugin list` shows whether guardrails is installed and enabled. If a guard does not
fire when you expect it to, see [Troubleshooting](troubleshooting.md).
