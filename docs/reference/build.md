# Build

The hooks are one Go program, built without cgo into a static binary. From a checkout of the
repository:

```sh
CGO_ENABLED=0 go build -o guardrails ./cmd/guardrails
CGO_ENABLED=0 go vet ./cmd/guardrails/...
CGO_ENABLED=0 go test ./cmd/guardrails/...
claude plugin validate --strict .
```

CI runs `go vet` and `go test` on `./cmd/guardrails/...` with `CGO_ENABLED=0` on Linux and
macOS, along with the launcher test scripts. It tests memory with cgo on Linux amd64 and
macOS arm64, runs the context-guard module tests with `claude plugin test` on Claude Code
2.1.317, and runs `shellcheck -s sh` on all three `run.sh` launchers on Linux.

`claude plugin validate --strict .` checks the marketplace file at the repository root.
Validate a plugin with `claude plugin validate --strict plugins/<name>`.

To try a local build in place of the downloaded one, set `GUARDRAILS_BIN` to its path (see
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

## Docs site

The site is built with [zensical](https://zensical.org/) from `mkdocs.yml` and `docs/`. Its
[Threat model](threat-model.md) page is generated from `THREAT_MODEL.md` at the repository
root, so generate it before a local build (the file is gitignored):

```sh
.github/scripts/threat-model-page.sh THREAT_MODEL.md docs/reference/threat-model.md
uvx zensical@0.0.74 build --strict
```

The pages workflow does the same on every push to `main` that touches the docs, the threat
model or the Go code, and adds the coverage badge's `coverage.json` (see
[Tests and coverage](testing.md#how-the-badge-is-made)).
