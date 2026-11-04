# Doctor

`claude-tools doctor` checks that the guardrails install can do its job. The launcher runs
`doctor -hook` at the end of the `SessionStart` hook. A healthy install prints nothing. A
problem prints one message that you see in the transcript and that Claude can read, so it can
help fix it. Doctor always exits 0 in that mode, so it can never block a session.

Run it by hand for a full report, with `ok`, `skip` and problem lines. By hand it exits 1 when
a check failed.

```
CLAUDE_PLUGIN_ROOT=<plugin dir> /path/to/claude-tools doctor
```

## Checks

| Check | Level | Passes when |
| --- | --- | --- |
| plugin enabled | `FAIL`, `WARN` | `enabledPlugins` in your user, project or local settings names `guardrails` and does not set it false. A marketplace install with no entry at all gives a `WARN`: managed settings are not read. |
| hooks reachable | `FAIL` | Every command in the plugin's `hooks/hooks.json` can start: the launcher script exists, and the subcommand it names is one this binary has. |
| version | `FAIL` | The binary carries a version stamp, and it equals the `version` in `plugin.json`. |
| binary | `FAIL` | The binary's sha256 equals the one the launcher recorded in `claude-tools.sha256` beside it after it checked the release archive. A binary with no record passes. |
| duplicate guard | `WARN` | No hook in your settings runs a subcommand the plugin already runs: `pre-tool-use`, `read-dedup`, `large-read-guard`, `retry-breaker`, `config-gate`, `session-env`, `receipt`, `gate` or `compact-guard`. A settings entry plus the plugin runs that hook twice. |
| rtk | `WARN` | If a settings hook runs `rtk`, it is on `PATH` and answers `rtk gain`. Another tool is also called `rtk`, and it does not. |

Without `CLAUDE_PLUGIN_ROOT` (the binary run by hand or as a status line) the plugin checks
are skipped.

## Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-hook` | off | Print only problems, as one JSON reply, and exit 0. |
| `-settings a,b` | user, project and local settings | Settings files to read. The user file is under `CLAUDE_CONFIG_DIR` when set. |
| `-plugin-root DIR` | `CLAUDE_PLUGIN_ROOT` | The plugin's install directory. |

## Version output

`claude-tools version` prints the release and the commit, such as `0.2.0 (5b8331f)`. Release
builds stamp both. A build made by `go install ...@v0.2.0` reports the release from the module
version, and a build in a git checkout reports the commit, with `-dirty` for uncommitted
changes. `unknown` means none of these was available, and doctor fails the version check.
