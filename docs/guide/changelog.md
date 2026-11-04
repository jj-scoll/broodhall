# Changelog

guardrails, memory and context-guard share one version; see
[Versioning](updating.md#versioning). rtk is versioned in
[jj-scoll/rtk](https://github.com/jj-scoll/rtk). Each release's full notes are on
[GitHub Releases](https://github.com/jj-scoll/broodhall/releases).

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
