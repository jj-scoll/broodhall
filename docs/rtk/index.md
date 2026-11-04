# RTK

<span class="badge badge-note">NOTE</span> at session start: Claude learns how rtk-condensed command output reads, and you learn when the rtk on `PATH` cannot condense it.

!!! why "Why it exists"
    [RTK](https://github.com/jj-scoll/rtk) rewrites commands such as `git status` to
    `rtk git status`, whose output is shorter. Claude reads that output better when it knows
    the output is condensed and how to get the raw result back. `rtk init` writes that note
    into `CLAUDE.md`. This plugin injects it at session start instead, and checks that the
    rtk it describes is installed.

## Install

```sh
claude plugin marketplace add jj-scoll/broodhall
claude plugin install rtk@broodhall
```

The plugin lives in the [jj-scoll/rtk](https://github.com/jj-scoll/rtk) fork, in its
`claude-plugin/` directory. Claude Code checks out only that directory, from the fork's
`develop` branch.

Requirements:

- **rtk from the jj-scoll fork on `PATH`.** The plugin does not ship the binary.
- **For the rewrite itself, the guardrails [`rtk` option](../guardrails/options.md#rtk).**
  This plugin does not rewrite commands. Claude Code runs PreToolUse hooks in parallel and
  keeps the last rewrite, so a second rewrite hook would race the guardrails one. Do not also
  install RTK's own Claude Code hook.

If `CLAUDE.md` already includes the text `rtk init` wrote (an `@RTK.md` line or an RTK
section), remove it once this plugin is on, or Claude reads both.

## Install the rtk binary

The fork has no release binaries, so build it from source with Rust's `cargo`, Rust 1.91 or
newer (see [rustup.rs](https://rustup.rs) if you have none):

```sh
cargo install --git https://github.com/jj-scoll/rtk --branch develop rtk
```

The trailing `rtk` names the package; without it cargo stops with "multiple packages with
binaries found". The binary lands in `~/.cargo/bin`, which must be on your `PATH`. Check it
with `rtk --version` and `rtk gain`. Run the same command with `--force` to update it.

Upstream install methods (`brew install rtk`, upstream's `install.sh`) give upstream rtk,
which works but lacks the fork's filters; the plugin says so at session start.

## Turn it off

- **The rewrite only:** turn off the guardrails `rtk` option with
  `/plugin configure guardrails@broodhall`.
- **The plugin:** `claude plugin disable rtk@broodhall`, or remove it with
  `claude plugin uninstall rtk@broodhall`.
- **The binary:** `cargo uninstall rtk`.

## What it does

At session start the hook checks the rtk on `PATH` and replies with one of these:

| rtk on `PATH` | Claude gets the awareness text | You see |
|---|---|---|
| The jj-scoll fork | yes | nothing |
| Upstream rtk | yes | `rtk plugin: the rtk on PATH is upstream rtk, ...` |
| Answers but fails `rtk gain` (another tool named rtk) | no | `rtk plugin: the rtk on PATH does not answer ...` |
| None | no | `rtk plugin: rtk is not on PATH, ...` |

The message is a `systemMessage`: Claude Code shows it in the transcript and does not send it
to Claude. The hook always exits 0, so it never blocks a session.

The fork is told apart from upstream by its `duckdb` filter: `rtk duckdb --help` succeeds
only on the fork. If upstream ever adds a `duckdb` command, upstream rtk will pass as the
fork.

The awareness text is the default level of `hooks/rtk-awareness.md` in the fork. The plugin
keeps its own copy, because Claude Code fetches only `claude-plugin/`, and a test in the fork
fails if the copy drifts.
