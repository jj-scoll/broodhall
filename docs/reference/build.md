# Build

The hooks are one Go program, built without cgo into a static binary. From a checkout of the
repository:

```sh
CGO_ENABLED=0 go build -o claude-tools ./cmd/claude-tools
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go test ./...
claude plugin validate --strict .
```

CI runs `go vet ./...` and `go test ./...` with `CGO_ENABLED=0` on Linux and macOS, and runs
`shellcheck -s sh` on the plugin's `run.sh` launcher on Linux.

`claude plugin validate --strict .` checks the marketplace and the plugin manifest at the
repository root.

To try a local build in place of the downloaded one, set `CLAUDE_TOOLS_BIN` to its path (see
[Options](../guardrails/options.md)).
