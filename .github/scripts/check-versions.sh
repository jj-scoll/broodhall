#!/bin/bash
# check-versions.sh [plugin.json...]
# Fails unless every plugin.json holds the same, valid X.Y.Z version. All plugins share one
# version (and the release tag is v<that version>), so a change that bumps one alone is wrong.
# With no arguments it checks plugins/*/.claude-plugin/plugin.json under the repo root.
set -eu
if [ "$#" -eq 0 ]; then
	cd "$(dirname "$0")/../.."
	set -- plugins/*/.claude-plugin/plugin.json
fi
re='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
first=
rc=0
for f in "$@"; do
	v=$(jq -r '.version // empty' "$f")
	if ! [[ $v =~ $re ]]; then
		echo "$f: version '$v' is not X.Y.Z" >&2
		rc=1
	fi
	echo "$f $v"
	[ -n "$first" ] || first=$v
	if [ "$v" != "$first" ]; then
		echo "$f: version $v differs from $first; all plugins share one version" >&2
		rc=1
	fi
done
exit $rc
