#!/bin/bash
# Tests for .github/scripts/changelog.sh.
#   changelog.sh stamp <X.Y.Z> [file]    autorelease: "## Unreleased" becomes "## X.Y.Z"
#   changelog.sh check <base-ref> [file] CI: a PR that changes shipped files adds an Unreleased entry
here=$(cd "$(dirname "$0")/.." && pwd)
cl=$here/changelog.sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
pass=0 fail=0
ok() { pass=$((pass + 1)); echo "ok   $1"; }
bad() { fail=$((fail + 1)); echo "FAIL $1"; }

head_txt='# Changelog

Intro.
'
old='## 1.0.0

- old entry. (#1)
'
notes='https://github.com/jj-scoll/broodhall/releases/tag/v'

# stamps <name> <input> <version> <expected>
stamps() {
	printf '%s' "$2" >"$tmp/cl.md"
	if ! "$cl" stamp "$3" "$tmp/cl.md" >"$tmp/out" 2>&1; then bad "$1: exited nonzero: $(cat "$tmp/out")"; return; fi
	printf '%s' "$4" >"$tmp/want.md"
	if diff -u "$tmp/want.md" "$tmp/cl.md" >"$tmp/diff"; then ok "$1"; else bad "$1:"; cat "$tmp/diff"; fi
}

stamps "Unreleased becomes the version" "$head_txt
## Unreleased

- new thing. (#9)

$old" 1.1.0 "$head_txt
## 1.1.0

- new thing. (#9)

$old"

stamps "no Unreleased: a placeholder entry, so every version is listed" "$head_txt
$old" 1.0.1 "$head_txt
## 1.0.1

- No user-facing changes; see the [release notes](${notes}1.0.1).

$old"

stamps "an empty Unreleased gets the placeholder too" "$head_txt
## Unreleased

$old" 1.0.1 "$head_txt
## 1.0.1

- No user-facing changes; see the [release notes](${notes}1.0.1).

$old"

stamps "a version already present is left alone" "$head_txt
## 1.0.0

- old entry. (#1)
" 1.0.0 "$head_txt
## 1.0.0

- old entry. (#1)
"

stamps "only the top Unreleased heading counts" "$head_txt
## Unreleased

- a. (#9)

## 1.0.0

- mentions ## Unreleased in text. (#1)
" 1.1.0 "$head_txt
## 1.1.0

- a. (#9)

## 1.0.0

- mentions ## Unreleased in text. (#1)
"

printf '%s' "$head_txt" >"$tmp/cl.md"
if "$cl" stamp 1.2 "$tmp/cl.md" >/dev/null 2>&1; then bad "a version that is not X.Y.Z is refused"; else ok "a version that is not X.Y.Z is refused"; fi
if "$cl" stamp 1.2.3 "$tmp/missing.md" >/dev/null 2>&1; then bad "a missing file is refused"; else ok "a missing file is refused"; fi
if "$cl" frobnicate >/dev/null 2>&1; then bad "an unknown subcommand is refused"; else ok "an unknown subcommand is refused"; fi

# check: a throwaway repository with main holding a changelog and one shipped file per area.
repo=$tmp/repo
git init -q -b main "$repo"
cd "$repo" || exit 1
git config user.email t@example.com
git config user.name t
mkdir -p docs/guide cmd/x plugins/p/.claude-plugin plugins/p/test
printf '%s\n%s' "$head_txt" "$old" >docs/guide/changelog.md
echo 'package x' >cmd/x/x.go
echo '{"version": "1.0.0"}' >plugins/p/.claude-plugin/plugin.json
echo 'test' >plugins/p/test/t.sh
echo 'readme' >README.md
git add -A
git commit -qm base

# pr <name> <want: pass|fail> <change...>: each change is a shell snippet run on a fresh branch.
pr() {
	name=$1 want=$2
	shift 2
	git checkout -q -B pr main
	for c in "$@"; do eval "$c"; done
	git add -A
	git commit -qm pr --allow-empty
	if "$cl" check main >"$tmp/out" 2>&1; then got=pass; else got=fail; fi
	if [ "$got" = "$want" ]; then ok "$name"; else bad "$name: want $want, got $got: $(cat "$tmp/out")"; fi
	git checkout -q main
}
add_unreleased='sed -i "s/^## 1.0.0$/## Unreleased\n\n- new. (#9)\n\n## 1.0.0/" docs/guide/changelog.md'

pr "docs only: no entry needed" pass 'echo more >>README.md'
pr "shipped change with an Unreleased entry" pass 'echo "// y" >>cmd/x/x.go' "$add_unreleased"
pr "shipped change with no changelog change" fail 'echo "// y" >>cmd/x/x.go'
pr "shipped change that only edits an old entry" fail 'echo "// y" >>cmd/x/x.go' 'sed -i "s/old entry/old entry, fixed/" docs/guide/changelog.md'
pr "shipped change with an empty Unreleased heading" fail 'echo "// y" >>cmd/x/x.go' 'sed -i "s/^## 1.0.0$/## Unreleased\n\n## 1.0.0/" docs/guide/changelog.md'
pr "plugin tests are not shipped" pass 'echo more >>plugins/p/test/t.sh'
pr "plugin.json version bumps are not shipped" pass 'echo "{}" >plugins/p/.claude-plugin/plugin.json'
pr "plugins/ is shipped" fail 'echo more >plugins/p/hooks.json'
pr "go.mod is shipped" fail 'echo "module m" >go.mod'

# main already has an unreleased entry (merged, release not cut yet).
git checkout -q main
sed -i "s/^## 1.0.0$/## Unreleased\n\n- earlier. (#8)\n\n## 1.0.0/" docs/guide/changelog.md
git commit -qam "earlier"
pr "adds to an existing Unreleased" pass 'echo "// y" >>cmd/x/x.go' 'sed -i "s/^- earlier. (#8)$/- earlier. (#8)\n- new. (#9)/" docs/guide/changelog.md'
pr "leaves an existing Unreleased as it was" fail 'echo "// y" >>cmd/x/x.go'

echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
