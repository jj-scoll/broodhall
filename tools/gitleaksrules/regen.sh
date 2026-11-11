#!/bin/sh
# Regenerate the vendored gitleaks rules and prove the detector still matches the library.
#
#   sh tools/gitleaksrules/regen.sh            same gitleaks version as go.mod (rebuilds the data)
#   sh tools/gitleaksrules/regen.sh v8.31.0    move to another gitleaks tag first
#
# Steps, each of which stops the script when it fails:
#   1. (with a TAG) go get that gitleaks version into this module.
#   2. Run gitleaks' own rule generator, built from a copy of the module in which Validate
#      records every rule's true-positive and false-positive examples (patch/validate.go.txt)
#      instead of checking them, three times (the examples are random samples). It also
#      checks that the generator reproduces the library's embedded default config.
#   3. generate: write cmd/guardrails/secretrules/rules_gen.go from the default config.
#   4. parity: run the library and cmd/guardrails/secretrules over the examples, the guardrails
#      test fixtures, mutations and mixes of them, and every file of this repository; fail on
#      any difference in (rule, secret); write testdata/parity.json.gz, the golden file
#      `go test ./cmd/guardrails/secretrules` checks without the library.
#
# Review the diff of rules_gen.go (new rules, changed regexes) before committing it.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
cd "$here"

if [ $# -ge 1 ]; then
	go get "github.com/zricethezav/gitleaks/v8@$1"
	go mod tidy
fi

mod=$(go list -m -f '{{.Dir}}' github.com/zricethezav/gitleaks/v8)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# gitleaks' rule generator, built from a copy of the module whose Validate records the examples
cp -R "$mod" "$tmp/gitleaks"
chmod -R u+w "$tmp/gitleaks"
cp "$here/patch/validate.go.txt" "$tmp/gitleaks/cmd/generate/config/utils/validate.go"
cp go.mod "$tmp/go.mod"
cp go.sum "$tmp/go.sum"
go mod edit -modfile="$tmp/go.mod" -replace "github.com/zricethezav/gitleaks/v8=$tmp/gitleaks" -replace "github.com/jj-scoll/broodhall=$root"
go build -modfile="$tmp/go.mod" -o "$tmp/genconfig" github.com/zricethezav/gitleaks/v8/cmd/generate/config
# the generator reads its template from its own directory
(
	cd "$tmp/gitleaks/cmd/generate/config"
	for _ in 1 2 3; do
		GITLEAKSRULES_EXAMPLES="$tmp/examples.jsonl" "$tmp/genconfig" "$tmp/default.toml"
	done
)

# the guardrails tests' own fixtures (secretgitleaks_test.go), as parity inputs
(cd "$root" && CGO_ENABLED=0 PARITY_FIXTURES_OUT="$tmp/fixtures.json" go test ./cmd/guardrails -run '^TestWriteParityFixtures$' -count=1 >/dev/null)

go run . generate -toml "$tmp/default.toml"
go run . parity -examples "$tmp/examples.jsonl" -fixtures "$tmp/fixtures.json" -repo "$root" -write
