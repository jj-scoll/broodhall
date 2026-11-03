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
