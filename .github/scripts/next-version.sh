#!/bin/bash
# next-version.sh <current-version>... < commit-messages
# Prints the next shared release version. Arguments are the plugins' current versions (plain
# X.Y.Z); stdin is the commits since the last tag, each starting with an RS (0x1e) and holding
# its full message, as `git log --no-merges --format=%x1e%B` writes them. Input with no RS is
# one commit. The first line of a commit is its title.
#
# The base is the highest current version. The bump is the largest of:
#   major  a title "type!:" / "type(scope)!:", "#major" in a title, or a footer line
#          "BREAKING CHANGE: " / "BREAKING-CHANGE: " (line start, colon, space) in a body
#   minor  a title "feat:" / "feat(scope):", "#minor" in a title, or plugin versions that are
#          not all equal (the lockstep is broken, so the release realigns them with a minor bump)
#   patch  anything else
# Prose in a body never counts, only titles and the footer. A "Merge pull request #N from ..."
# commit is skipped whole: the PR's own commits drive the bump, not the merge or the PR title.
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
found=$(awk -v RS='\036' '
	{
		n = split($0, l, "\n")
		title = l[1]
		if (title ~ /^Merge pull request #[0-9]+ from /) next
		if (title ~ /^[a-z]+(\([^)]*\))?!:/ || title ~ /#major/) major = 1
		if (title ~ /^feat(\([^)]*\))?:/ || title ~ /#minor/) minor = 1
		for (i = 2; i <= n; i++) if (l[i] ~ /^BREAKING[ -]CHANGE: /) major = 1
	}
	END { print major ? "major" : minor ? "minor" : "" }
' <<<"$log")
case $found in
major) level=major ;;
minor) [ "$level" = patch ] && level=minor ;;
esac

IFS=. read -r maj min pat <<<"$highest"
case $level in
major) echo "$((maj + 1)).0.0" ;;
minor) echo "$maj.$((min + 1)).0" ;;
patch) echo "$maj.$min.$((pat + 1))" ;;
esac
