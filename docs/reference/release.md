# Release

Releases are automatic. guardrails, context-guard and memory share one version, and every
release bumps every `plugins/*/.claude-plugin/plugin.json` to the same `X.Y.Z`. The tag is
the plain `vX.Y.Z`. Claude Code delivers a plugin update only when that plugin's version
changes, which is why all three move together.

## What a merge does

A push to `main` that changes shipped files (`plugins/` except `test/` and `tests/`, `cmd/`,
`go.mod`, `go.sum`, `.claude-plugin/`) runs the `autorelease` workflow. Docs-only and
`.github`-only changes do not. The workflow:

1. computes the next version with `.github/scripts/next-version.sh`,
2. rewrites every `plugin.json` to it,
3. commits `release vX.Y.Z [skip ci]` to `main` and pushes it with the tag in one atomic push
   (if `main` moved, it starts over from the new tip),
4. calls the `release` workflow for that tag, which builds the assets and creates the GitHub
   release with generated notes.

Only one release runs at a time; a merge that lands during a release waits for it.

## Which number

The base is the highest current plugin version. The bump is the largest that applies to the
commit messages since the last `v*` tag:

| Bump | When |
|---|---|
| major | a `BREAKING CHANGE` line, a `type!:` title (for example `feat!:`), or `#major` anywhere |
| minor | a `feat:` or `feat(scope):` title line, `#minor` anywhere, or plugin versions that differ (the release realigns them) |
| patch | everything else |

Pull requests are merged with a merge commit, so the PR title is a line of the commit
message and counts as a title. Put `#minor` or `#major` in the PR title or description to
force a bump.

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
release was already created, delete it first.

## Setup

`main` must accept the `autorelease` push from `github-actions[bot]`. With branch protection
or a ruleset that requires pull requests or status checks, add `github-actions` as a bypass
actor, or the push is rejected. Under Settings, Actions, General, workflow permissions must
allow the token to write (the workflows request `contents: write` themselves).

## Earlier memory releases

Before 0.4.0 memory released separately under `memory-v<version>` tags (`memory-v0.1.0`).
That tag scheme and its workflow are gone. Installs of memory 0.1.0 keep working because
they use `memory-v0.1.0`'s assets, which are still in place.
