# Release

1. Bump `version` in `plugins/guardrails/.claude-plugin/plugin.json` and commit.
2. Push a tag `v<version>`, for example `v0.2.0` for version `0.2.0`.

The `release` workflow runs on any tag that starts with `v`. It stops if the tag does not
match the `version` in `plugin.json`, then runs `go test ./...` with `CGO_ENABLED=0`. It builds
a static binary for linux and darwin on amd64 and arm64 and publishes a GitHub release with
four archives, `claude-tools_<os>_<arch>.tar.gz`, plus `checksums.txt`.

An installed plugin sees the new version on update. At the next session start its launcher
downloads that version's archive and `checksums.txt` from the release, checks the sha256, and
keeps the binary under the plugin's data directory.

## memory

memory releases separately, with its own tag.

1. Bump `version` in `plugins/memory/.claude-plugin/plugin.json` and commit.
2. Push a tag `memory-v<version>`, for example `memory-v0.1.0` for version `0.1.0`.

The `release-memory` workflow stops if the tag does not match `version` in `plugin.json`.
The `v*` filter of the `release` workflow does not fire on a `memory-v` tag, so the two
releases never mix. The workflow then downloads the DuckDB extensions for each platform,
runs the memory tests with cgo on, and builds on four native runners: Linux amd64 and arm64
(on Ubuntu 22.04, which sets the glibc floor at 2.35), and macOS amd64 and arm64.

Each build packs the program and `ext/` into a tar. The publish job compresses each with
`xz -9e`, writes `checksums.txt` (it stops unless there are exactly four archives) and
publishes a GitHub release with four archives, `memory_<os>_<arch>.tar.xz`, plus
`checksums.txt`.

An installed plugin sees the new version on update. At the next session start its launcher
downloads that version's archive in the background and checks the sha256, as for guardrails.
