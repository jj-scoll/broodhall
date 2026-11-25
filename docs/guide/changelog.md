# Changelog

guardrails, memory and context-guard share one version; see
[Versioning](updating.md#versioning). rtk is versioned in
[jj-scoll/rtk](https://github.com/jj-scoll/rtk). Each release's full notes are on
[GitHub Releases](https://github.com/jj-scoll/broodhall/releases).

## 1.12.7

- guardrails: when the handoff writer's `claude -p` call fails, its log line now names the failure (`subtype`, `terminal_reason`, `errors`, `result`) instead of a cut of the JSON that ended inside `usage`. (#86)

## 1.12.6

- guardrails: the Bash guards read a command as the shell will run it, so respelling no longer
  slips past them. `\rm`, `r''m`, line continuations, `c=rm; "$c"`, `$'\x72\x6d'`,
  `"$(printf ...)"`, `{rm,-rf,~}`, `${IFS}`, a function the line defines, code piped or
  sourced into a shell (`printf ... | sh`, `base64 -d | sh`, `source <(...)`), the shell
  commands of `python -c` and `perl -e` one-liners, and paths such as `/home/../home/you` or
  `/hom[e]/you` all get the plain command's answer. A command name or piped code only known at
  run time asks (`dynamic-command`, `opaque-shell`). See [Bash core](../guardrails/bash-core.md). (#85)
- guardrails: `git push origin +main` blocks like `--force`; recursive `chmod` of a protected
  target blocks with any mode, not only `777`; `dd` blocks only when it writes to a device
  (`of=/dev/...`), so `dd if=/dev/zero of=disk.img` passes. (#85)
- guardrails: new asks, with no switch: `terraform`/`tofu`/`terragrunt destroy` and
  `apply -destroy` (`iac-destroy`, which replaces the plan note for a destroy),
  `aws s3 rm --recursive` and `s3 rb --force` (`s3-rm`), `find -delete` and `find -exec rm`
  (`find-delete`). `git push --delete` and `git push origin :branch` ask under the `git`
  family (`git-push-delete`). (#85)
- guardrails: commands that only look destructive pass. `git push --dry-run` (`-n`) with `+ref`,
  `--force`, `--delete` or `:ref`; `--help` or `--version` on `chmod`, `chown` and `rm`, and
  `-h`, `-help` or `--help` on `terraform` and `tofu` `destroy`; `bash -n`, `sh -n` and `zsh -n`, which
  parse and run nothing; and `find ... -delete` or `-exec rm` on a start path the `rm` rule
  allows (`find ./dist -delete`), which asks only where `rm -rf` of that path blocks. (#85)

## 1.12.5

- memory: a daemon that exits or fails to start no longer deletes the socket of another daemon
  that has since bound the same path, which left a running daemon unreachable (every client got
  "no such file"). The socket is removed on shutdown only while it is still the one that
  daemon created, and a start-up dial that times out against a busy daemon is reported as "cannot
  verify" instead of being treated as a stale socket and removed.
- memory: `get_tool_uses` prints a nested object or list on the lines below its key, indented,
  instead of running the first child onto the key's line.
- memory: `get_tool_uses` shows a payload with text after its closing brace or bracket
  (`{"a":1} extra`) as plain text, as it already did for trailing JSON.

## 1.12.4

- No user-facing changes; see the [release notes](https://github.com/jj-scoll/broodhall/releases/tag/v1.12.4).

## 1.12.3

- guardrails, memory: no change in behavior; the Python-compatible text helpers and the container-run parser both binaries carried as identical copies now live in `internal/pytext` and `internal/container`.

## 1.12.2

- guardrails: the new [`ecs_export`](../guardrails/options.md#ecs_export) option, off by
  default, also writes each guard decision as one Elastic Common Schema JSON line to
  `~/.local/state/guardrails/events.ecs.jsonl` (or
  [`ecs_export_path`](../guardrails/options.md#ecs_export_path)), so a SIEM agent such as
  Wazuh can ship it. An event holds the kind, the rule id, the session id, the host, the user
  and the binary version. It never holds command text, paths or values. (#77)
- guardrails: `GUARDRAILS_WRITER_CONFIG_DIR` picks the account the handoff writer's `claude -p`
  calls run as, whichever account the session is on, as `MEMORY_OBSERVER_CONFIG_DIR` does for
  the memory observer. See [Write handoff](../guardrails/write-handoff.md#login-model-and-cost).

## 1.12.1

- guardrails: a [threat model](../reference/threat-model.md) maps threats T01 to T16 to the
  OWASP Top 10 for LLM Applications (2025) and MITRE ATLAS, names the guard for each, and
  lists the bypasses left open on purpose. Tests check the document against the code. (#72)
- guardrails: the [egress guard](../guardrails/egress.md) also scans WebFetch URLs for
  secrets, outside email addresses and phone numbers, and WebSearch queries for secrets.
  (#72)
- guardrails: a `-v` that belongs to the wrapped command, as in `command docker run -v …` or
  `command rm -rf -v /`, no longer makes the line look like a `command -v` lookup, so the
  guards check the command inside. A shell function declaration with no name no longer
  crashes the parser. Fuzz tests found both. (#72)
- Release: every release asset has a signed SLSA build provenance attestation. Check one with
  `gh attestation verify <file> --repo jj-scoll/broodhall`; see
  [Release](../reference/release.md). (#72)
- Docs: an [architecture](../reference/architecture.md) diagram, a
  [testing](../reference/testing.md) page and a coverage badge. (#72)

## 1.12.0

- memory: raw search over past sessions, with no tokens spent. Prompts, Claude's replies, tool
  inputs and file paths are indexed. `search` takes `source: "raw"`, `branch` and `model`, and
  falls back to raw search when no note matches. A new MCP tool, `read_session`, returns a
  masked conversation in order. (#71)
- memory: stricter capture rules. A call that reads a credential file or runs a credential
  command (`gh auth token`, `kubectl get secret`, …) is kept without its output.
  `capture_exclude_dirs` and `capture_tools=false` keep only the tool name. The rules apply
  to what is stored and to what is sent to the observer; see [Options](../memory/options.md).
  (#71)
- memory: `memory resume` finds a session across every config directory and prints the
  command to resume it (`--fork` to fork it). Forks are grouped under their original session
  in the CLI and the UI. (#71)
- memory: `memory import` reads past transcripts in the background through the same masking
  and capture rules, without running `claude` or queueing summaries. Running it again adds only
  what is new. (#71)

## 1.11.1

- guardrails: the gitleaks rules run from vendored data on a small built-in detector instead
  of the gitleaks library. Detections are unchanged, `pre-tool-use` is back to about 5 ms p50
  from about 27 ms, and the binary is 30% smaller. (#70)

## 1.11.0

- guardrails: `.env` and `.env.*` (except `.example`, `.sample` and `.template`), `.npmrc`,
  `.pypirc`, `.git-credentials`, `*.tfvars` and `*.pem` are
  [protected](../guardrails/secret-files.md) by name in any directory. `~/.kube/config` and
  the gcloud credential files are added to the home list. (#69)
- guardrails: secret-value detection also runs the gitleaks v8.30.1 default rules, except
  `generic-api-key`. (#69)
- guardrails: a re-issued masked read through guardrails' own `redact --secrets` wrapper no
  longer asks. (#69)
- memory: an older running daemon is asked to stop and the new one takes over after an
  update. `memory gc` removes old version directories. After the first update to this version,
  restart the old daemon once by hand. (#69)
- memory: projects are keyed by their normalised `origin` URL, falling back to the directory
  name. Notes under an old key are mapped to the new one when every recorded session sits
  inside the repository. (#69)
- Release: each release commits its assets' checksums into the plugins, and the launchers
  refuse a download that does not match. (#69)

## 1.10.8

- The rtk plugin is pinned to a commit in the marketplace instead of following `develop`.
  (#68)

## 1.10.7

- guardrails: the [PII guard](../guardrails/pii.md) runs `psql` with `--html`, so each result
  is its own table and is masked under its own column names, even when two results have the
  same number of columns. `-o`/`--output` keep the format the user asked for. (#67)

## 1.10.6

- guardrails: `psql` output with several results, or `\echo` and `\pset` lines, no longer
  crashes the [PII guard](../guardrails/pii.md) and loses the output. Each result is masked by
  its own header. (#65)

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
