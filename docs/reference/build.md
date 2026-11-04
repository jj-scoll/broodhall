# Build

The hooks are one Go program, built without cgo into a static binary. From a checkout of the
repository:

```sh
CGO_ENABLED=0 go build -o guardrails ./cmd/guardrails
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

## memory

memory is built with cgo, because DuckDB needs it. `CGO_ENABLED=0 go build ./...` fails on
`./cmd/memory`, so keep the guardrails commands above pointed at `./cmd/guardrails`.

```sh
CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" -o memory ./cmd/memory
CGO_ENABLED=1 MEMORY_EXT_DIR=/path/to/ext go test ./cmd/memory/...
```

The tests load three signed DuckDB extensions, `fts`, `httpfs` and `quack`, from
`MEMORY_EXT_DIR`, and stop with a clear message if one is missing. Each is published as
`https://extensions.duckdb.org/<duckdb version>/<platform>/<name>.duckdb_extension.gz`.
The DuckDB version must match the one linked by `duckdb-go` in `go.mod`.

Because of cgo, each platform is built on a native runner, not cross-compiled. To use a local
build, set `MEMORY_BIN` to its path; the launcher then skips the download and the daemon
uses its default extension directory unless you set `MEMORY_EXT_DIR`.
