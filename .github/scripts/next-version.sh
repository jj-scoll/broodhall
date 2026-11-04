#!/bin/bash
# next-version.sh <current-version>... < commit-messages
# Prints the next shared release version. Arguments are the plugins' current versions (plain
# X.Y.Z); stdin is the full text of the commits since the last tag (git log --format=%B).
#
# The base is the highest current version. The bump is the largest of:
#   major  a line "BREAKING CHANGE"/"BREAKING-CHANGE", a "type!:" title, or "#major"
#   minor  a title "feat:" / "feat(scope):", "#minor", or plugin versions that are not all
#          equal (the lockstep is broken, so the release realigns them with a minor bump)
#   patch  anything else
set -eu
[ "$#" -gt 0 ] || { echo "usage: next-version.sh <version>... < commit-messages" >&2; exit 2; }
re='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
for v in "$@"; do
	[[ $v =~ $re ]] || { echo "not X.Y.Z: $v" >&2; exit 2; }
done
highest=$(printf '%s\n' "$@" | sort -t. -k1,1n -k2,2n -k3,3n | tail -n 1)
distinct=$(printf '%s\n' "$@" | sort -u | wc -l)
log=$(cat)

level=patch
[ "$distinct" -gt 1 ] && level=minor
if grep -Eq '^feat(\([^)]*\))?:|#minor' <<<"$log"; then level=minor; fi
if grep -Eq '^BREAKING[ -]CHANGE|^[a-z]+(\([^)]*\))?!:|#major' <<<"$log"; then level=major; fi

IFS=. read -r maj min pat <<<"$highest"
case $level in
major) echo "$((maj + 1)).0.0" ;;
minor) echo "$maj.$((min + 1)).0" ;;
patch) echo "$maj.$min.$((pat + 1))" ;;
esac
