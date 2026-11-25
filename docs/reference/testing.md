# Tests and coverage

The README's coverage badge is measured on every docs deploy from `main`: the
[pages workflow](https://github.com/jj-scoll/broodhall/blob/main/.github/workflows/pages.yml)
runs `go test ./...` with a coverage profile and publishes the total as
[`coverage.json`](https://jj-scoll.github.io/broodhall/coverage.json) on this site.

## The numbers

Measured at commit `5836629` with Go 1.27.3 on linux/amd64. The badge has the current total.

| Package | Statement coverage |
| --- | --- |
| `cmd/guardrails` | 88.9% |
| `cmd/guardrails/secretfiles` | 94.1% |
| `cmd/guardrails/secretrules` | 97.8% |
| `cmd/memory` | 82.1% |
| `tools/hooklatency` (a benchmark tool, no tests) | 0.0% |
| **Total** (`go tool cover -func`) | **85.1%** |

The repository has 33,080 lines of Go outside tests and 26,003 lines of Go tests, in 128 and
119 files (`git ls-files '*.go'`, split on `_test.go`). That count includes the
`tools/gitleaksrules` generator, which is its own Go module and so is not in `./...`, and one
generated file, `cmd/guardrails/secretrules/rules_gen.go` (271 lines).

## What kinds of tests there are

| Kind | Where | What it proves |
| --- | --- | --- |
| Unit | `cmd/*/*_test.go`, 784 `Test` functions | The shell parser, each guard's rules, redaction, the memory database and its queries. |
| Hook end to end | `cmd/guardrails`, through `runHook` in `hookharness_test.go` | A hook subcommand gets Claude Code's JSON on stdin; the test checks its exit code, stdout and stderr exactly as Claude Code would read them. |
| Corpus | `cmd/guardrails/testdata/guard-corpus.tsv`, `TestGuardCorpus` | A frozen table of commands and the decision each must get, kept from the original bash rules, with every intended difference marked. |
| Parity | `cmd/guardrails/secretrules/testdata/parity.json.gz` | The vendored gitleaks secret rules find what the gitleaks library itself found on the same texts. |
| Threat model | `cmd/guardrails/threatmodel_test.go` | One attack per threat in the [threat model](threat-model.md), fed to the real hook entry point; each admitted bypass is pinned as still open; and the document, the tests it cites and the ATLAS and OWASP IDs are checked against each other. |
| Launcher | `plugins/*/test/*.sh` | `run.sh` with no binary, a failed download, a checksum mismatch, a crashing or hung binary: guardrails' guards fail closed, memory fails open, and a binary that failed its checksum is never run. |
| context-guard module | `plugins/context-guard/tests/context-guard.test.ts` | The hooks module, run by `claude plugin test` in Claude Code's own test host. |
| Release scripts | `.github/scripts/test/*.sh` | Version bumps, checksum pinning, release drafting and packing, the coverage badge and this site's copy of the threat model. |

## Run them

```sh
# guardrails: no cgo, no setup
CGO_ENABLED=0 go test ./cmd/guardrails/...

# everything, with coverage. memory needs cgo and the signed DuckDB extensions (see Build).
MEMORY_EXT_DIR=/path/to/ext go test ./... -coverprofile=cover.out
go tool cover -func=cover.out | tail -1

# the launcher and script suites CI runs
bash plugins/guardrails/test/run_test.sh
bash plugins/guardrails/test/install_test.sh
bash plugins/memory/test/run_test.sh
bash plugins/context-guard/test/run_test.sh
for t in .github/scripts/test/*_test.sh; do bash "$t"; done

# the threat-model tests alone
go test ./cmd/guardrails -run TestThreat -v
```

[Build](build.md) says where the DuckDB extensions come from. CI runs the Go tests and the
plugin launcher suites on Linux and macOS, and the context-guard and release-script suites on
Linux; see [`ci.yml`](https://github.com/jj-scoll/broodhall/blob/main/.github/workflows/ci.yml).

## How the badge is made

The pages workflow's build job runs the coverage step before it builds the site. The step
downloads the DuckDB extensions, runs `go test ./...` with `-coverprofile`, and pipes
`go tool cover -func` through `.github/scripts/coverage-badge.sh`, which writes a
[shields.io endpoint](https://shields.io/badges/endpoint-badge) JSON. The file is copied into
the built site and deployed with it. No external coverage service, account or token is
involved.

The badge never holds back the docs. If the download, a test or the measurement fails, the
step is marked failed but the job carries on, and the deploy republishes the badge that is
live on the site, so the badge only ever shows a number from a passing run.
