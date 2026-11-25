#!/bin/sh
# changelog.sh stamp <X.Y.Z> [file]
#   autorelease, in the release commit: the top "## Unreleased" section becomes "## X.Y.Z". With
#   no Unreleased entries (a release of tests or internals only), "## X.Y.Z" gets a one-line
#   placeholder, so every released version is listed. A version already present is left alone.
# changelog.sh check <base-ref> [file]
#   CI, on a pull request: when the branch changes shipped files, its top "## Unreleased"
#   section must hold an entry that the merge base does not. Shipped files are the ones that make
#   autorelease cut a release; keep the pathspec in step with autorelease.yml.
# file defaults to docs/guide/changelog.md.
set -eu

usage() {
	echo "usage: changelog.sh stamp <X.Y.Z> [file] | check <base-ref> [file]" >&2
	exit 2
}

# The non-blank lines of the top "## Unreleased" section on stdin, if the first "## " heading is it.
unreleased() {
	awk '/^## / { if (seen || $0 !~ /^## Unreleased[ \t]*$/) exit; seen = 1; next } seen && NF'
}

[ "$#" -ge 2 ] || usage
cmd=$1 arg=$2 file=${3:-docs/guide/changelog.md}

case "$cmd" in
stamp)
	echo "$arg" | grep -Eqx '[0-9]+\.[0-9]+\.[0-9]+' || { echo "changelog.sh: version must be X.Y.Z, got '$arg'" >&2; exit 2; }
	[ -f "$file" ] || { echo "changelog.sh: $file: no such file" >&2; exit 1; }
	if grep -qx "## $arg" "$file"; then
		echo "changelog.sh: $file already has ## $arg"
		exit 0
	fi
	if [ -n "$(unreleased <"$file")" ]; then empty=0; else empty=1; fi
	placeholder="- No user-facing changes; see the [release notes](https://github.com/jj-scoll/broodhall/releases/tag/v$arg)."
	tmp=$(mktemp)
	awk -v v="$arg" -v ph="$placeholder" -v empty="$empty" '
		!done && /^## / {
			done = 1
			print "## " v
			if ($0 ~ /^## Unreleased[ \t]*$/) {
				if (empty) { print ""; print ph }
				next
			}
			print ""; print ph; print ""
		}
		{ print }
		END { if (!done) { print "## " v; print ""; print ph } }
	' "$file" >"$tmp"
	cat "$tmp" >"$file"
	rm -f "$tmp"
	;;
check)
	base=$(git merge-base "$arg" HEAD)
	if git diff --quiet "$base" HEAD -- plugins .claude-plugin cmd go.mod go.sum \
		':(exclude)plugins/*/.claude-plugin/plugin.json' \
		':(exclude,glob)plugins/*/test/**' ':(exclude,glob)plugins/*/tests/**'; then
		echo "No shipped files changed; no changelog entry needed."
		exit 0
	fi
	new=$(git show "HEAD:$file" | unreleased)
	old=$(git show "$base:$file" 2>/dev/null | unreleased || true)
	if [ -n "$new" ] && [ "$new" != "$old" ]; then
		echo "Changelog entry found under ## Unreleased."
		exit 0
	fi
	echo "::error file=$file::This pull request changes shipped files, so merging it cuts a release. Add an entry under '## Unreleased' at the top of $file (create the heading if it is not there), or add the no-changelog label if nothing a user would notice changed."
	exit 1
	;;
*) usage ;;
esac
