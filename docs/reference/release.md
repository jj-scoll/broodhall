# Release

Releases are automatic. guardrails, context-guard and memory share one version, and every
release bumps every `plugins/*/.claude-plugin/plugin.json` to the same `X.Y.Z`. The tag is
the plain `vX.Y.Z`. Claude Code delivers a plugin update only when that plugin's version
changes, which is why all three move together.

## What a merge does

A push to `main` that changes shipped files (`plugins/` except `test/` and `tests/`, `cmd/`,
`go.mod`, `go.sum`, `.claude-plugin/`) runs the `autorelease` workflow. Docs-only and
`.github`-only changes do not. The workflow:

1. computes the next version with `.github/scripts/next-version.sh`, rewrites every
   `plugin.json` and makes the `release vX.Y.Z [skip ci]` commit locally, pushing it only to
   a scratch branch (`autorelease-candidate`), so nothing is advertised yet,
2. calls the `release` workflow on that commit, which runs the tests, builds every asset and
   creates a draft GitHub release with generated notes (drafts cannot be downloaded),
3. pushes the commit and the tag `vX.Y.Z` to `main` in one atomic push, then publishes the
   draft. If `main` moved meanwhile, it rebases when only docs, tests or `.github` changed;
   if shipped files changed, it releases nothing, because the run queued for that merge does,
4. deletes the scratch branch and any draft left over from a failed run.

Guardrails' hooks fail closed when the binary for the `plugin.json` version cannot be
downloaded, which is why `main` is only updated after the assets exist. The gap between the
push and the publish is seconds.

Only one release runs at a time; a merge that lands during a release waits for it.

## Which number

The base is the highest current plugin version. The bump is the largest that applies to the
commit messages since the last `v*` tag:

| Bump | When |
|---|---|
| major | a `type!:` or `type(scope)!:` title (for example `feat!:`), `#major` in a title, or a `BREAKING CHANGE: ` / `BREAKING-CHANGE: ` footer |
| minor | a `feat:` or `feat(scope):` title, `#minor` in a title, or plugin versions that differ (the release realigns them) |
| patch | everything else |

Only a commit's title (first line) counts, apart from the breaking-change footer, which must
start a line in the body and be followed by a colon and a space (the Conventional Commits
footer). Prose in a body that mentions `#major` or `feat:` does nothing.

Pull requests are merged with a merge commit. The workflow reads the commits since the last tag
with `--no-merges`, so the "Merge pull request #N from ..." commit, and with it the PR title
and description, never counts, while the PR's own commits do. To force a bump, put `#minor`
or `#major` in the title of one of the PR's commits, or use a `feat:` or `type!:` title.

## Assets

The `release` workflow stops unless the tag equals `v<version>` and all three versions are
equal, then runs the tests and builds:

- `claude-tools_<os>_<arch>.tar.gz` (guardrails): static binaries for linux and darwin on
  amd64 and arm64, built with `CGO_ENABLED=0`.
- `memory_<os>_<arch>.tar.xz` (memory): the program and `ext/` (the DuckDB extensions it
  loads), built with cgo on four native runners: Linux amd64 and arm64 (on Ubuntu 22.04, which
  sets the glibc floor at 2.35), and macOS amd64 and arm64. The tests run with the extensions
  loaded first.
- `checksums.txt`: sha256 of all eight archives (the publish job stops unless there are exactly
  eight). Each launcher reads only the line for its own archive.

An installed plugin sees the new version on update. At the next session start its launcher
downloads `v<version>/<archive>` and `checksums.txt` from the release, checks the sha256, and
keeps the binary under the plugin's data directory. memory installs in the background and
fails open; guardrails installs in the foreground.

## Pull requests

A pull request must not bump one plugin alone. `ci` runs `.github/scripts/check-versions.sh`
and the Go test `TestPluginVersionsShared`; both fail unless the three versions are equal
semver. Do not bump versions by hand in a PR: the release does it.

## Manual release

The `release` workflow also runs when you push a `v*` tag. Set all three `plugin.json`
versions to the same `X.Y.Z`, commit, then tag that commit `vX.Y.Z` and push the tag.

To rebuild a release whose workflow failed after the tag was pushed, run the `release`
workflow by hand (Actions, release, Run workflow) with the existing tag. If the GitHub
release was already created, delete it first. A failed automatic run leaves `main` and the tags
as they were; re-run it, or merge the next change.

## Setup

`main` must accept the `autorelease` push from `github-actions[bot]`. With branch protection
or a ruleset that requires pull requests or status checks, add `github-actions` as a bypass
actor, or the push is rejected. The default workflow permission can stay read-only: each job
that writes declares `contents: write` itself.

## Earlier memory releases

Before 1.0.0 memory released separately under `memory-v<version>` tags (`memory-v0.1.0`).
That tag scheme and its workflow are gone. Installs of memory 0.1.0 keep working because
they use `memory-v0.1.0`'s assets, which are still in place.
