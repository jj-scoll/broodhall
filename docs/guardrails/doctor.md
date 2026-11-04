# Doctor

`guardrails doctor` checks that the guardrails install can do its job. The launcher runs
`doctor -hook` at the end of the `SessionStart` hook. A healthy install prints nothing. A
problem prints one message that you see in the transcript and that Claude can read, so it can
help fix it. Doctor always exits 0 in that mode, so it can never block a session.

Run it by hand for a full report, with `ok`, `skip` and problem lines. By hand it exits 1 when
a check failed.

```
CLAUDE_PLUGIN_ROOT=<plugin dir> /path/to/guardrails doctor
```

## Checks

| Check | Level | Passes when |
| --- | --- | --- |
| plugin enabled | `FAIL`, `WARN` | `enabledPlugins` in your user, project or local settings names `guardrails` and does not set it false. Managed settings files (`managed-settings.json` and `managed-settings.d/*.json` in the system directory) count too. A marketplace install with no entry in any of them gives a `WARN`. |
| hooks reachable | `FAIL` | Every command in the plugin's `hooks/hooks.json` can start: the launcher script exists, and the subcommand it names is one this binary has. |
| version | `FAIL` | The binary carries a version stamp, and it equals the `version` in `plugin.json`. |
| binary | `FAIL` | The binary's sha256 equals the one the launcher recorded in `guardrails.sha256` beside it after it checked the release archive. A binary with no record passes. |
| duplicate guard | `WARN` | No hook in your settings runs a subcommand the plugin already runs: `pre-tool-use`, `read-dedup`, `large-read-guard`, `retry-breaker`, `ask-outcome`, `config-gate`, `session-env`, `receipt`, `gate` or `compact-guard`. A settings entry plus the plugin runs that hook twice. |
| rtk | `WARN` | If a settings hook runs `rtk`, it is on `PATH` and answers `rtk gain`. Another tool is also called `rtk`, and it does not. |
| plugin versions | `WARN` | The installed broodhall plugins (guardrails, memory, context-guard) in `plugins/installed_plugins.json` under `CLAUDE_CONFIG_DIR` are all at one version, and each equals the version the marketplace checkout offers. The message names the plugins that are behind and the `claude plugin update` command for each. |
| statusLine | `WARN` | The `statusLine` command in your settings does not run the binary under its old name, `claude-tools`. That name stops getting updates. Use the `bin/*/guardrails` command from [Status line](statusline.md). |

Without `CLAUDE_PLUGIN_ROOT` (the binary run by hand or as a status line) the plugin checks
are skipped. The plugin versions and statusLine checks do not need it.

Claude Code does not document the exact layout of `installed_plugins.json`, only that each
record has a `scope`, `installPath` and `version`. Doctor reads a `plugins` map from
`name@marketplace` to a list of records, and ignores a record without a version. A marketplace
counts as broodhall's when it is named `broodhall` or `known_marketplaces.json` records
`jj-scoll/broodhall` as its source. A plugin whose marketplace entry names an external source
has no version on disk to compare, so it is skipped. See [Roll out to a team](../guide/team.md#version-skew).

## Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-hook` | off | Print only problems, as one JSON reply, and exit 0. |
| `-settings a,b` | user, project and local settings | Settings files to read. The user file is under `CLAUDE_CONFIG_DIR` when set. |
| `-plugin-root DIR` | `CLAUDE_PLUGIN_ROOT` | The plugin's install directory. |

## Version output

`guardrails version` prints the release and the commit, such as `X.Y.Z (5b8331f)`. Release
builds stamp both. A build made by `go install ...@vX.Y.Z` reports the release from the module
version, and a build in a git checkout reports the commit, with `-dirty` for uncommitted
changes. `unknown` means none of these was available, and doctor fails the version check.
