#!/bin/bash
# draft-release.sh publish|cleanup <tag>     (GITHUB_REPOSITORY and GH_TOKEN from the environment)
#   publish  publishes the draft release for <tag>. Retries transient API errors, and a release
#            already published counts as done: a 502 can come back from a PATCH that applied.
#   cleanup  for a run that did not publish. Once the tag is on the remote, main advertises the
#            version, so the draft is published, never deleted. Only a 404 for the tag deletes
#            the draft; a tag state it cannot read fails the run and keeps the draft.
# v1.5.2 is why: its push landed, one publish call got HTTP 502, and cleanup deleted the draft,
# so main advertised a version with no assets and guardrails' hooks failed closed.
set -eu
usage() { echo "usage: draft-release.sh publish|cleanup <tag>" >&2; exit 2; }
[ "$#" -eq 2 ] || usage
mode=$1
export TAG=$2
repo=$GITHUB_REPOSITORY
tries=5

drafts() { gh api "repos/$repo/releases" --paginate --jq '.[] | select(.draft and .tag_name == env.TAG) | .id'; }

publish() {
	local attempt id
	for attempt in $(seq "$tries"); do
		# Published already (this or an earlier PATCH applied): done. The endpoint is documented
		# to return published releases only; .draft is checked anyway.
		if id=$(gh api "repos/$repo/releases/tags/$TAG" --jq 'select(.draft | not) | .id' 2>/dev/null) && [ -n "$id" ]; then
			echo "release $TAG is published"
			return 0
		fi
		# By id: gh may not find a draft by tag.
		if id=$(drafts | head -n 1) && [ -n "$id" ] &&
			gh api -X PATCH "repos/$repo/releases/$id" -F draft=false >/dev/null; then
			echo "published the draft release $TAG"
			return 0
		fi
		echo "publishing $TAG failed (attempt $attempt of $tries)" >&2
		[ "$attempt" -lt "$tries" ] && sleep $((attempt * 5))
	done
	echo "::error::could not publish the draft release $TAG; publish it by hand: gh release edit $TAG --draft=false" >&2
	return 1
}

# tag_state: prints present or absent; fails when the API keeps erroring
tag_state() {
	local attempt out
	for attempt in $(seq "$tries"); do
		if out=$(gh api "repos/$repo/git/ref/tags/$TAG" 2>&1); then
			echo present
			return 0
		fi
		case $out in *"HTTP 404"*) echo absent; return 0 ;; esac
		echo "reading tag $TAG failed (attempt $attempt of $tries): $out" >&2
		[ "$attempt" -lt "$tries" ] && sleep $((attempt * 5))
	done
	return 1
}

case $mode in
publish) publish ;;
cleanup)
	state=$(tag_state) || { echo "::error::cannot tell whether $TAG is on the remote; keeping its draft" >&2; exit 1; }
	if [ "$state" = present ]; then
		echo "$TAG is on the remote, so main advertises it: publishing its draft instead of deleting it"
		publish
	else
		ids=$(drafts) # a failed listing fails the run, not an empty loop
		for id in $ids; do
			gh api -X DELETE "repos/$repo/releases/$id"
		done
	fi
	;;
*) usage ;;
esac
