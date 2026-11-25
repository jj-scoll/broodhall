#!/bin/sh
# Each pull request writes its changelog entry to its own file in dir, never to a shared
# "## Unreleased" section: two open pull requests, or a pull request and a release, then never edit
# the same lines, so the entry cannot be lost resolving a merge conflict.
#
# changelog.sh stamp <X.Y.Z> [file] [dir]
#   autorelease, in the release commit: the entry files (dir/*.md except README.md) become a
#   "## X.Y.Z" section above the newest version in file, in file-name order, and are removed.
#   With none (a release of tests or internals only), "## X.Y.Z" gets a one-line placeholder, so
#   every released version is listed. A version already present is left alone, as are the files.
# changelog.sh check <base-ref> [dir]
#   CI, on a pull request: when the branch changes shipped files, it must add an entry file to dir
#   whose first non-blank line is a "- " bullet. Shipped files are the ones that make autorelease
#   cut a release; keep the pathspec in step with autorelease.yml.
# file defaults to docs/guide/changelog.md, dir to changelog.d.
set -eu
export LC_ALL=C

usage() {
	echo "usage: changelog.sh stamp <X.Y.Z> [file] [dir] | check <base-ref> [dir]" >&2
	exit 2
}

# Its input without leading or trailing blank lines.
trim() {
	awk 'NF { for (; n > 0; n--) print ""; print; p = 1; next } p { n++ }'
}

[ "$#" -ge 2 ] || usage
cmd=$1 arg=$2

case "$cmd" in
stamp)
	file=${3:-docs/guide/changelog.md} dir=${4:-changelog.d}
	echo "$arg" | grep -Eqx '[0-9]+\.[0-9]+\.[0-9]+' || { echo "changelog.sh: version must be X.Y.Z, got '$arg'" >&2; exit 2; }
	[ -f "$file" ] || { echo "changelog.sh: $file: no such file" >&2; exit 1; }
	if grep -qx "## $arg" "$file"; then
		echo "changelog.sh: $file already has ## $arg"
		exit 0
	fi
	body=
	for f in "$dir"/*.md; do
		if [ -f "$f" ] && [ "${f##*/}" != README.md ]; then
			text=$(trim <"$f")
			if [ -n "$text" ]; then body=${body:+$body
}$text; fi
		fi
	done
	[ -n "$body" ] || body="- No user-facing changes; see the [release notes](https://github.com/jj-scoll/broodhall/releases/tag/v$arg)."
	# ENVIRON, not awk -v, which would turn a backslash in an entry into an escape.
	CL_VERSION=$arg CL_BODY=$body
	export CL_VERSION CL_BODY
	tmp=$(mktemp)
	awk '
		!done && /^## / { done = 1; print "## " ENVIRON["CL_VERSION"]; print ""; print ENVIRON["CL_BODY"]; print "" }
		{ print; last = $0 }
		END {
			if (done) exit
			if (NR && last != "") print ""
			print "## " ENVIRON["CL_VERSION"]; print ""; print ENVIRON["CL_BODY"]
		}
	' "$file" >"$tmp"
	cat "$tmp" >"$file"
	rm -f "$tmp"
	for f in "$dir"/*.md; do
		if [ -f "$f" ] && [ "${f##*/}" != README.md ]; then rm -f "$f"; fi
	done
	;;
check)
	dir=${3:-changelog.d}
	dir=${dir%/}
	base=$(git merge-base "$arg" HEAD)
	if git diff --quiet "$base" HEAD -- plugins .claude-plugin cmd go.mod go.sum \
		':(exclude)plugins/*/.claude-plugin/plugin.json' \
		':(exclude,glob)plugins/*/test/**' ':(exclude,glob)plugins/*/tests/**'; then
		echo "No shipped files changed; no changelog entry needed."
		exit 0
	fi
	added=$(git diff --name-only --no-renames --diff-filter=A "$base" HEAD -- "$dir")
	found='' bad=''
	while IFS= read -r f; do
		case "$f" in "$dir"/*/* | "$dir"/README.md | "") continue ;; "$dir"/*.md) ;; *) continue ;; esac
		first=$(git show "HEAD:$f" | awk 'NF { print; exit }')
		case "$first" in
		"- "*) found="$found $f" ;;
		*)
			echo "::error file=$f::$f must hold one '- ' bullet per user-facing change, starting on its first line."
			bad=1
			;;
		esac
	done <<EOF
$added
EOF
	[ -z "$bad" ] || exit 1
	if [ -n "$found" ]; then
		echo "Changelog entry file:$found"
		exit 0
	fi
	echo "::error::This pull request changes shipped files, so merging it cuts a release. Add a new file to $dir/, named after your branch (e.g. $dir/fix-socket.md), holding one '- ' bullet per user-facing change; see $dir/README.md. Or add the no-changelog label if nothing a user would notice changed."
	exit 1
	;;
*) usage ;;
esac
