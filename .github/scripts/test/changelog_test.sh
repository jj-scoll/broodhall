#!/bin/bash
# Tests for .github/scripts/changelog.sh.
#   changelog.sh stamp <X.Y.Z> [file] [dir] autorelease: the entry files in dir become "## X.Y.Z"
#   changelog.sh check <base-ref> [dir]     CI: a PR that changes shipped files adds an entry file
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
frag=$tmp/frag

# frags <name=content>...: a fresh entry folder holding exactly these files.
frags() {
	rm -rf "$frag"
	mkdir -p "$frag"
	for kv in "$@"; do printf '%s' "${kv#*=}" >"$frag/${kv%%=*}"; done
}

# stamps <name> <input> <version> <expected> [files left in the folder afterwards]
stamps() {
	printf '%s' "$2" >"$tmp/cl.md"
	if ! "$cl" stamp "$3" "$tmp/cl.md" "$frag" >"$tmp/out" 2>&1; then bad "$1: exited nonzero: $(cat "$tmp/out")"; return; fi
	printf '%s' "$4" >"$tmp/want.md"
	if ! diff -u "$tmp/want.md" "$tmp/cl.md" >"$tmp/diff"; then bad "$1:"; cat "$tmp/diff"; return; fi
	left=$( (cd "$frag" 2>/dev/null && ls -A) | tr '\n' ' ')
	if [ "$left" = "${5:-}" ]; then ok "$1"; else bad "$1: folder holds '$left', want '${5:-}'"; fi
}

frags 'b.md=- second. (#9)
' 'a.md=- first. (#8)
  continued.
'
stamps "entry files become the version, sorted by name, and are removed" "$head_txt
$old" 1.1.0 "$head_txt
## 1.1.0

- first. (#8)
  continued.
- second. (#9)

$old"

rm -rf "$frag"
stamps "no folder: a placeholder entry, so every version is listed" "$head_txt
$old" 1.0.1 "$head_txt
## 1.0.1

- No user-facing changes; see the [release notes](${notes}1.0.1).

$old"

frags 'README.md=How to add an entry.
'
stamps "only the README: the placeholder, and the README stays" "$head_txt
$old" 1.0.1 "$head_txt
## 1.0.1

- No user-facing changes; see the [release notes](${notes}1.0.1).

$old" "README.md "

frags 'a.md=

'
stamps "an empty entry file: the placeholder, and the file is removed" "$head_txt
$old" 1.0.1 "$head_txt
## 1.0.1

- No user-facing changes; see the [release notes](${notes}1.0.1).

$old"

frags 'a.md=- new. (#9)
'
stamps "a version already present is left alone, and so are the entry files" "$head_txt
$old" 1.0.0 "$head_txt
$old" "a.md "

frags 'a.md=- no newline at the end. (#8)' 'b.md=- trailing blanks. (#9)


'
stamps "missing final newlines and trailing blank lines do not change the layout" "$head_txt
$old" 1.1.0 "$head_txt
## 1.1.0

- no newline at the end. (#8)
- trailing blanks. (#9)

$old"

# shellcheck disable=SC2016 # literal backticks and backslashes are the point
frags 'a.md=- `\n` and `a & b` and `\\` stay as written. (#9)
'
stamps "backslashes and ampersands are copied as written" "$head_txt
$old" 1.1.0 "$head_txt
## 1.1.0

- \`\\n\` and \`a & b\` and \`\\\\\` stay as written. (#9)

$old"

frags 'a.md=- new. (#9)
'
stamps "no heading yet: the version goes at the end" "$head_txt" 1.1.0 "$head_txt
## 1.1.0

- new. (#9)
"

frags 'a.md=- new. (#9)
' 'notes.txt=not an entry
'
stamps "files that are not .md are ignored" "$head_txt
$old" 1.1.0 "$head_txt
## 1.1.0

- new. (#9)

$old" "notes.txt "

printf '%s' "$head_txt" >"$tmp/cl.md"
if "$cl" stamp 1.2 "$tmp/cl.md" "$frag" >/dev/null 2>&1; then bad "a version that is not X.Y.Z is refused"; else ok "a version that is not X.Y.Z is refused"; fi
if "$cl" stamp 1.2.3 "$tmp/missing.md" "$frag" >/dev/null 2>&1; then bad "a missing file is refused"; else ok "a missing file is refused"; fi
if "$cl" frobnicate x >/dev/null 2>&1; then bad "an unknown subcommand is refused"; else ok "an unknown subcommand is refused"; fi

# check: a throwaway repository with main holding a changelog and one shipped file per area.
repo=$tmp/repo
git init -q -b main "$repo"
cd "$repo" || exit 1
git config user.email t@example.com
git config user.name t
mkdir -p docs/guide cmd/x plugins/p/.claude-plugin plugins/p/test changelog.d
printf '%s\n%s' "$head_txt" "$old" >docs/guide/changelog.md
echo 'package x' >cmd/x/x.go
echo '{"version": "1.0.0"}' >plugins/p/.claude-plugin/plugin.json
echo 'test' >plugins/p/test/t.sh
echo 'readme' >README.md
echo 'How to add an entry.' >changelog.d/README.md
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
ship='echo "// y" >>cmd/x/x.go'
add_entry='printf "%s\n" "- new. (#9)" >changelog.d/my-branch.md'

pr "docs only: no entry needed" pass 'echo more >>README.md'
pr "shipped change with a new entry file" pass "$ship" "$add_entry"
pr "shipped change with no entry file" fail "$ship"
pr "shipped change with an empty entry file" fail "$ship" 'printf "\n\n" >changelog.d/my-branch.md'
pr "an entry file must start with a '- ' bullet" fail "$ship" 'printf "## Unreleased\n\n- new.\n" >changelog.d/my-branch.md'
pr "the README is not an entry" fail "$ship" 'echo more >>changelog.d/README.md'
pr "a file that is not .md is not an entry" fail "$ship" 'echo "- new." >changelog.d/my-branch.txt'
pr "an Unreleased section in the changelog no longer counts" fail "$ship" 'sed -i "s/^## 1.0.0$/## Unreleased\n\n- new. (#9)\n\n## 1.0.0/" docs/guide/changelog.md'
pr "one bad entry file fails even beside a good one" fail "$ship" "$add_entry" 'printf "\n" >changelog.d/other.md'
pr "plugin tests are not shipped" pass 'echo more >>plugins/p/test/t.sh'
pr "plugin.json version bumps are not shipped" pass 'echo "{}" >plugins/p/.claude-plugin/plugin.json'
pr "plugins/ is shipped" fail 'echo more >plugins/p/hooks.json'
pr "go.mod is shipped" fail 'echo "module m" >go.mod'

# main already holds an entry file (merged, release not cut yet).
git checkout -q main
echo "- earlier. (#8)" >changelog.d/earlier.md
git add -A
git commit -qm "earlier"
pr "edits only an entry file main already has" fail "$ship" 'echo "- more. (#9)" >>changelog.d/earlier.md'
pr "adds its own beside one main already has" pass "$ship" "$add_entry"

echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
