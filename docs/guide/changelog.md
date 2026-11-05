# Changelog

guardrails, memory and context-guard share one version; see
[Versioning](updating.md#versioning). rtk is versioned in
[jj-scoll/rtk](https://github.com/jj-scoll/rtk). Each release's full notes are on
[GitHub Releases](https://github.com/jj-scoll/broodhall/releases).

## 1.10.5

- guardrails: the [secret-file](../guardrails/secret-files.md) and
  [settings-file](../guardrails/settings-files.md) guards cover every Claude config
  directory: the session's `CLAUDE_CONFIG_DIR`, whatever its name, and any directory in home
  whose name starts with `.claude`. Both follow `$CLAUDE_CONFIG_DIR` and a symlinked home or
  `CLAUDE_CONFIG_DIR`, and the settings guard also follows `cd` and `env -C`. The secret
  guard also protects the PII key under `XDG_STATE_HOME`. Before, only `~/.claude` and
  `~/.claude-work` were known by name. (#58)
- guardrails: the [skipped-hooks ask](../guardrails/git.md#skipped-hooks-and-signing-ask) reads
  `GIT_CONFIG_PARAMETERS` the way git does, so the `'core.hooksPath'='/dev/null'` form that
  git 2.31 and later writes asks too, and a value that only mentions `core.hooksPath` no
  longer asks. (#58)
- guardrails: two sessions that find a dead install lock no longer both take it over and both
  download. (#59)
- guardrails: when the data directory cannot be written, a guard's BLOCKED message says the
  binary cannot be installed, instead of "Installing it in the background now" or "The last
  install failed". (#62)
- memory: `memory version` prints the release and commit, and the MCP server reports the
  release version instead of `0.1.0`. A data directory it cannot write is logged as `cannot
  write <data>/bin; memory is off this session` instead of failing silently. (#62)
- memory: the handoff skill names the plugin's work-state tool,
  `mcp__plugin_memory_memory__work_state_write`, and the SessionStart context no longer says
  local files are copied into `deliverables/`. (#60)
- Docs: the fixes from a full docs audit, including the privacy page, the hooks reference,
  observe mode and the release process. (#60)

## 1.10.4

- memory: two sessions that install the binary at the same time download it once, also
  where `mkdir` is uutils (Ubuntu 25.10 and later). (#57)

## 1.10.3

- guardrails: two sessions that install the binary at the same time download it once, also
  where `mkdir` is uutils (Ubuntu 25.10 and later). (#56)

## 1.10.2

- guardrails: an install into a data directory that cannot be written says "cannot write"
  instead of "another install is running". (#55)
- guardrails: `team-init` names the settings file under `CLAUDE_CONFIG_DIR`. (#55)

## 1.10.1

- guardrails: an install with no `curl` on `PATH` says "curl not found" instead of a failed
  download. (#54)
- Docs: how to [run guardrails from a terminal](../guardrails/index.md#run-it-from-a-terminal), and the install
  failure reasons in [Troubleshooting](troubleshooting.md). (#54)

## 1.10.0

- guardrails: the [status line](../guardrails/statusline.md#the-rc-segment) shows `⇄ rc` while
  Remote Control is connected. (#52)
- memory: one daemon reads the transcripts of every Claude config directory into one
  database, whichever account started it. (#53)

## 1.9.0

- guardrails: [`/guardrails:setup`](../guardrails/setup.md) walks you through a first
  configuration, and `team-init` prints settings for a
  [team rollout](team.md). (#49)
- guardrails: doctor warns when the broodhall plugins are at different versions or behind the
  marketplace ([Version skew](team.md#version-skew)). (#49)

## 1.8.0

- memory: saves each compaction summary, so search finds it, through a new `PostCompact` hook
  ([Hooks](../reference/hooks.md#memory)). (#48)

## 1.7.1

- guardrails: after an update, the guards run the previous version's binary until the new
  one downloads, instead of blocking. (#50)
- memory: after an update, the MCP server serves from the previous version's binary, or
  waits for the download, so its tools are not lost for the session
  ([Updating](updating.md#between-the-update-and-the-download)). (#50)

## 1.7.0

- guardrails: config files, state files and environment variables move to the `guardrails`
  name. The `claude-tools` names keep working ([Older names](../guardrails/options.md#older-names)). (#51)
- Release: publishing the draft release retries, and a draft is never deleted once its tag
  is on `main`. (#47)

## 1.6.0

- guardrails: the binary is renamed from `claude-tools` to `guardrails`, along with its
  release assets. (#45)

## 1.5.3

- rtk: installs over HTTPS, so no GitHub SSH key is needed. (#44)

## 1.5.2

1.5.2 has no GitHub release: its draft was deleted after the tag was pushed, so its binaries
cannot be downloaded. Update to 1.5.3 or later. 1.7.0 keeps this from happening again.

- memory: `run.sh cost` finds the plugin data directory when run from a terminal. (#40)
- guardrails: `write-handoff` keeps background jobs that differ only in digits apart, and
  keeps the count of jobs it left out. (#42)

## 1.5.1

- memory: `memory cost` prices `claude-opus-4-0`, and a price entry no longer matches a model
  name that continues with more digits. (#37)

## 1.5.0

- memory: records the token counts of every response and adds `memory cost`, a report of
  cost per day and session and of large cache writes
  ([Token cost](../memory/index.md#token-cost-and-cache-misses)). (#36)

## 1.4.0

- guardrails: the sweep guard asks before git sweeps up another live session's files
  ([Shared checkout](../guardrails/shared-tree.md)). (#34)
- memory: marks notes whose files changed since they were written
  ([Stale notes](../memory/index.md#stale-notes)). (#35)

## 1.3.0

- guardrails: `guard-report` shows whether asks ran after they were asked. (#33)

## 1.2.0

- guardrails: [observe mode](../guardrails/options.md#observe) and `guard-report`. (#31)
- guardrails: closes shell wrapper gaps, and asks on git hook bypass and on download-and-run
  ([Download and run](../guardrails/download-exec.md)). (#32)
- guardrails: read-dedup forgets its ledger when the session compacts. (#30)
- context-guard: declares guardrails as a plugin dependency, so installing it installs
  guardrails. (#29)

## 1.1.0

- memory: a redesigned local web viewer ([UI and Quack](../memory/ui-and-quack.md)). (#28)

## 1.0.1

- The marketplace adds the [rtk](../rtk/index.md) plugin, from the jj-scoll/rtk fork. (#24)
- Release: only commit titles and the breaking-change footer decide the version bump. (#25)

## 1.0.0

- Releases are automatic, and guardrails, memory and context-guard share one version. memory
  downloads from the same `vX.Y.Z` release as guardrails, and the `memory-v` tags end. (#23)

This was not a breaking change. The release script of the time read `#major` in the prose of
a commit body as a major bump; 1.0.1 fixed that. Nothing changed in the plugins' options or
behaviour between 0.3.3 and 1.0.0.

## Before 1.0.0

- **0.3.0 to 0.3.3:** guardrails gains the opt-in rtk rewrite, config-gate, the receipt, the
  handoff gate and compact-guard, write-handoff and autoclear, the status line and doctor.
  All guards are on by default and fail closed. context-guard 0.1.0 arrives. The patch
  releases fix checksum handling and the status line's handoff segment.
- **memory 0.1.0:** the first memory release, under its own `memory-v0.1.0` tag.
- **0.2.0:** the marketplace is renamed `broodhall` and the docs site starts.
- **0.1.0 to 0.1.2:** guardrails as a plugin, from the earlier `jj-scoll` marketplace.
