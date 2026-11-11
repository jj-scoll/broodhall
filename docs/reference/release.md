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
   `plugin.json`, removes the previous release's `plugins/*/checksums.txt` and makes the
   `release vX.Y.Z [skip ci]` commit locally, pushing it only to a scratch branch
   (`autorelease-candidate`), so nothing is advertised yet,
2. calls the `release` workflow on that commit, which runs the tests, builds every asset and
   creates a draft GitHub release with generated notes (drafts cannot be downloaded),
3. writes the built archives' sha256 into `plugins/guardrails/checksums.txt` and
   `plugins/memory/checksums.txt` (`.github/scripts/pin-checksums.sh`) and commits them as
   `release vX.Y.Z: checksums [skip ci]`, on top of the first commit,
4. pushes both commits and the tag `vX.Y.Z`, which points at the second, to `main` in one
   atomic push, then publishes the draft. If `main` moved meanwhile, it rebases when only
   docs, tests or `.github` changed; if shipped files changed, it releases nothing, because
   the run queued for that merge does,
5. deletes the scratch branch. A draft left over from a failed run is published if its tag
   reached `main`, and deleted only if the tag is not on the remote.

Guardrails' hooks fail closed when the binary for the `plugin.json` version cannot be
downloaded, which is why `main` is only updated after the assets exist. The gap between the
push and the publish is seconds.

## Checksums in git

The launchers install an archive only when its sha256 is the one in the `checksums.txt` of
their own plugin directory, which reaches the user through the git checkout, not through the
release ([How a download is checked](../guide/install.md#how-a-download-is-checked)). So the
hashes have to be in the commit that advertises the version.

They cannot be in the commit the archives are built from: each binary is stamped with that
commit's hash, so the archive's hash depends on the commit. Hence two commits. The first
bumps the version and is what gets built; the second records the hashes of what was built.
`main` moves from one release's second commit straight to the next one's, so no checkout has
a version without its hashes. The first commit has no `checksums.txt` at all, so no commit
pairs a version with another version's hashes, and the tests pass at both.

The file is `# version X.Y.Z`, comment lines, then one `<sha256>  <archive>` line for each of
the plugin's four archives. The Go test `TestPinnedChecksumsMatchPluginVersion` fails when a
`checksums.txt` in the tree is not for the `plugin.json` version or does not list exactly
those four. Do not edit the files by hand.

If writing or committing the files fails, the run stops before the push: `main` and the tags
stay as they were and the draft is deleted. A release whose tag commit has the files cannot
be rebuilt, because new archives have new hashes; the `release` workflow stops rather than
publish archives the launchers would refuse. Cut a new release instead.

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

The workflow reads the commits since the last tag with `git log --no-merges`, so which titles
count depends on how the pull request was merged. Squash merging is the convention, but the
repository also allows merge commits and rebase merges.

- **Squash, one commit.** The PR lands as one commit, and GitHub gives it that commit's own
  title, not the PR title. The commit title decides the bump.
- **Squash, two or more commits.** The PR lands as one commit with the PR title, and a body
  that lists each commit's message. The PR title decides the bump; the commit titles in the
  body are prose and do not count. A breaking-change footer in any of the commits is still
  at the start of a line in that body, so it still makes a major bump.
- **Merge commit.** The "Merge pull request #N from ..." commit is skipped, and with it the
  PR title. Each of the PR's own commits counts, by its title and footer, except merge
  commits inside the branch.
- **Rebase merge.** Each of the PR's commits lands on `main` as it is, and each counts. The PR
  title never does.

The title GitHub proposes for a squash merge can be edited before you confirm it, and the
edited title is the one that counts. To force a bump, put `#minor` or `#major` in the title
that lands (for a single-commit PR, that commit's title), or use a `feat:` or `type!:`
title.

## Assets

The `release` workflow stops unless the tag equals `v<version>` and all three versions are
equal, then runs the tests and builds:

- `guardrails_<os>_<arch>.tar.gz` (guardrails): static binaries for linux and darwin on
  amd64 and arm64, built with `CGO_ENABLED=0`. Older releases name it
  `claude-tools_<os>_<arch>.tar.gz`; each plugin version downloads from its own tag, so older
  versions keep working.
- `memory_<os>_<arch>.tar.xz` (memory): the program and `ext/` (the DuckDB extensions it
  loads), built with cgo on four native runners: Linux amd64 and arm64 (on Ubuntu 22.04, which
  sets the glibc floor at 2.35), and macOS amd64 and arm64. The tests run with the extensions
  loaded first.
- `checksums.txt`: sha256 of all eight archives (the publish job stops unless there are exactly
  eight). A launcher reads it, and only the line for its own archive, when its plugin
  directory has no `checksums.txt`; see [Checksums in git](#checksums-in-git).

An installed plugin sees the new version on update. At the next session start its launcher
downloads `v<version>/<archive>` from the release, checks the sha256 against the plugin
directory's `checksums.txt`, and keeps the binary under the plugin's data directory. memory installs in the background and
fails open; guardrails installs in the foreground.

## Pull requests

A pull request must not bump one plugin alone. `ci` runs `.github/scripts/check-versions.sh`
and the Go test `TestPluginVersionsShared`; both fail unless the three versions are equal
semver. Do not bump versions by hand in a PR: the release does it.

## Manual release

The `release` workflow also runs when you push a `v*` tag. Set all three `plugin.json`
versions to the same `X.Y.Z` and delete `plugins/guardrails/checksums.txt` and
`plugins/memory/checksums.txt` in the same commit, then tag that commit `vX.Y.Z` and push the
tag. The files hold the previous version's hashes: left in place they fail the release's
tests, and a launcher would refuse to install. A manual release therefore has no hashes in
git, and its launchers check against the release's own `checksums.txt`, which only shows that
the archive arrived whole. The next automatic release writes the files again.

To rebuild a release whose workflow failed after the tag was pushed, run the `release`
workflow by hand (Actions, release, Run workflow) with the existing tag. If the GitHub
release was already created, delete it first. This works for a manual release only; an
automatic one has its hashes in git and cannot be rebuilt (see
[Checksums in git](#checksums-in-git)). For a tag from before the binary was renamed
(its source has `cmd/claude-tools`), pick that tag under "Use workflow from", so the build
makes the `claude-tools_*` assets its launcher downloads. A failed automatic run leaves `main` and the tags
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
