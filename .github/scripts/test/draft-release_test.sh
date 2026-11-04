#!/bin/bash
# shellcheck disable=SC2016 # check's tests are eval'd after the run, so they are single-quoted
# Tests for .github/scripts/draft-release.sh publish|cleanup <tag>, against a stub gh.
#
# The case that broke v1.5.2: finalize's atomic push of the commit and tag landed, then one
# publish call got HTTP 502, and cleanup deleted the draft. Main advertised v1.5.2 with no
# release, and guardrails' hooks fail closed when the binary cannot be downloaded.
#   publish  retries transient errors, and counts a release that is already published as done
#            (a 502 can come back from a PATCH that did apply).
#   cleanup  deletes the draft only once the tag is known absent (404). With the tag on the
#            remote it publishes the draft instead, and on an unknown tag state it keeps it.
here=$(cd "$(dirname "$0")/.." && pwd)
dr=$here/draft-release.sh
pass=0 fail=0
ok() { pass=$((pass + 1)); echo "ok   $1"; }
bad() { fail=$((fail + 1)); echo "FAIL $1: $2"; }
check() { if eval "$2"; then ok "$1"; else bad "$1" "$3"; fi; } # check <name> <shell test> <why>

t=$(mktemp -d)
trap 'rm -rf "$t"' EXIT
mkdir -p "$t/bin"

# Stub gh. State lives in $S: draft (holds the draft's id), published, tag (on the remote);
# fail_<list|patch|pub|tagref|delete> hold how many HTTP 502s that call returns before it works;
# patch_lies makes one PATCH apply and still return 502. Every call is logged to $S/calls.
cat >"$t/bin/gh" <<'EOF'
#!/bin/bash
S=$STUB_STATE
echo "$*" >>"$S/calls"
tag=$(cat "$S/tagname")
fail() { f=$S/fail_$1; n=$(cat "$f" 2>/dev/null || echo 0)
	if [ "$n" -gt 0 ]; then echo $((n - 1)) >"$f"; echo "gh: Server Error (HTTP 502)" >&2; exit 1; fi; }
notfound() { echo "gh: Not Found (HTTP 404)" >&2; exit 1; }
[ "$1" = api ] || exit 99
shift
method=GET path= jq=.
while [ $# -gt 0 ]; do
	case $1 in
	-X) method=$2; shift 2 ;;
	--jq) jq=$2; shift 2 ;;
	--paginate) shift ;;
	-F | -f) shift 2 ;;
	*) path=$1; shift ;;
	esac
done
out() { jq -r "$jq" <<<"$1"; }
case "$method $path" in
"GET repos/o/r/releases")
	fail list
	j='[{"id":1,"draft":true,"tag_name":"v0.0.1"},{"id":2,"draft":false,"tag_name":"v0.0.2"}'
	[ -f "$S/draft" ] && j+=",{\"id\":$(cat "$S/draft"),\"draft\":true,\"tag_name\":\"$tag\"}"
	[ -f "$S/published" ] && j+=",{\"id\":9,\"draft\":false,\"tag_name\":\"$tag\"}"
	out "$j]" ;;
"GET repos/o/r/releases/tags/$tag")
	fail pub
	if [ -f "$S/published" ]; then out '{"id":9,"draft":false}'
	elif [ -f "$S/draft_by_tag" ]; then out "{\"id\":$(cat "$S/draft"),\"draft\":true}"
	else notfound; fi ;;
"PATCH repos/o/r/releases/"*)
	fail patch
	[ -f "$S/draft" ] && [ "${path##*/}" = "$(cat "$S/draft")" ] || notfound
	rm "$S/draft"; touch "$S/published"
	if [ -f "$S/patch_lies" ]; then rm "$S/patch_lies"; echo "gh: Server Error (HTTP 502)" >&2; exit 1; fi
	out '{}' ;;
"GET repos/o/r/git/ref/tags/$tag")
	fail tagref
	[ -f "$S/tag" ] || notfound
	out "{\"ref\":\"refs/tags/$tag\"}" ;;
"DELETE repos/o/r/releases/"*)
	fail delete
	[ -f "$S/draft" ] && [ "${path##*/}" = "$(cat "$S/draft")" ] || notfound
	rm "$S/draft"; echo "${path##*/}" >>"$S/deleted" ;;
*) echo "stub gh: unexpected $method $path" >&2; exit 98 ;;
esac
EOF
printf '#!/bin/sh\necho "$1" >>"$STUB_STATE/sleeps"\n' >"$t/bin/sleep"
chmod +x "$t/bin/gh" "$t/bin/sleep"

n=0
# fresh: a new state dir with a draft (id 7) for v1.2.3; prints its path
fresh() { n=$((n + 1)); S=$t/s$n; mkdir -p "$S"; echo v1.2.3 >"$S/tagname"; echo 7 >"$S/draft"; touch "$S/calls"; }
# run <mode>: runs the script against $S; sets rc, err
run() {
	PATH="$t/bin:$PATH" STUB_STATE=$S GITHUB_REPOSITORY=o/r "$dr" "$1" v1.2.3 >"$S/out" 2>"$S/err"
	rc=$?
	err=$(cat "$S/err")
}
patches() { grep -c '^api -X PATCH' "$S/calls"; }
sleeps() { [ -f "$S/sleeps" ] && wc -l <"$S/sleeps" || echo 0; }

# --- publish
fresh; run publish
check "publish: a draft is published" '[ "$rc" = 0 ] && [ -f "$S/published" ] && [ ! -e "$S/draft" ]' "rc=$rc err=$err"
check "publish: only our tag's draft is touched" 'grep -q "PATCH repos/o/r/releases/7" "$S/calls" && ! grep -q "releases/1" "$S/calls"' "$(cat "$S/calls")"

fresh; echo 2 >"$S/fail_list"; run publish
check "publish: retries a listing that 502s" '[ "$rc" = 0 ] && [ -f "$S/published" ] && [ "$(sleeps)" -ge 2 ]' "rc=$rc sleeps=$(sleeps) err=$err"

fresh; echo 2 >"$S/fail_patch"; run publish
check "publish: retries a PATCH that 502s" '[ "$rc" = 0 ] && [ -f "$S/published" ]' "rc=$rc err=$err"

fresh; touch "$S/patch_lies"; run publish
check "publish: a 502 from a PATCH that applied counts as published" '[ "$rc" = 0 ] && [ "$(patches)" = 1 ]' "rc=$rc patches=$(patches) err=$err"

fresh; touch "$S/draft_by_tag"; run publish
check "publish: a draft returned by tag is not taken as published" '[ "$rc" = 0 ] && [ -f "$S/published" ] && [ "$(patches)" = 1 ]' "rc=$rc patches=$(patches) err=$err"

fresh; rm "$S/draft"; touch "$S/published"; run publish
check "publish: an already published release is done" '[ "$rc" = 0 ] && [ "$(patches)" = 0 ]' "rc=$rc patches=$(patches) err=$err"

fresh; rm "$S/draft"; run publish
check "publish: no draft and no release fails" '[ "$rc" != 0 ] && printf %s "$err" | grep -q "v1.2.3"' "rc=$rc err=$err"

fresh; echo 99 >"$S/fail_patch"; run publish
check "publish: a PATCH that keeps failing fails, draft kept" '[ "$rc" != 0 ] && [ -f "$S/draft" ] && [ "$(patches)" = 5 ]' "rc=$rc patches=$(patches)"

# --- cleanup
fresh; run cleanup
check "cleanup: no tag on the remote: the draft is deleted" '[ "$rc" = 0 ] && [ ! -e "$S/draft" ] && [ ! -e "$S/published" ] && grep -qx 7 "$S/deleted"' "rc=$rc err=$err"

fresh; echo 1 >"$S/fail_list"; run cleanup
check "cleanup: no tag, a failed listing fails the run" '[ "$rc" != 0 ] && [ -f "$S/draft" ]' "rc=$rc"

fresh; rm "$S/draft"; run cleanup
check "cleanup: no tag, no draft: nothing to do" '[ "$rc" = 0 ] && [ ! -e "$S/deleted" ]' "rc=$rc err=$err"

fresh; touch "$S/tag"; run cleanup
check "cleanup: tag on the remote (the v1.5.2 case): publishes, never deletes" '[ "$rc" = 0 ] && [ -f "$S/published" ] && [ ! -e "$S/deleted" ]' "rc=$rc deleted=$(cat "$S/deleted" 2>/dev/null) err=$err"

fresh; touch "$S/tag"; rm "$S/draft"; touch "$S/published"; run cleanup
check "cleanup: tag on the remote, already published: nothing to do" '[ "$rc" = 0 ] && [ ! -e "$S/deleted" ] && [ "$(patches)" = 0 ]' "rc=$rc err=$err"

fresh; touch "$S/tag"; echo 99 >"$S/fail_patch"; run cleanup
check "cleanup: tag on the remote, publish keeps failing: fails, draft kept" '[ "$rc" != 0 ] && [ -f "$S/draft" ] && [ ! -e "$S/deleted" ]' "rc=$rc"

fresh; echo 2 >"$S/fail_tagref"; run cleanup
check "cleanup: retries a tag lookup that 502s, then deletes on 404" '[ "$rc" = 0 ] && [ ! -e "$S/draft" ] && grep -qx 7 "$S/deleted"' "rc=$rc err=$err"

fresh; echo 99 >"$S/fail_tagref"; run cleanup
check "cleanup: tag state unknown: fails, draft kept" '[ "$rc" != 0 ] && [ -f "$S/draft" ] && [ ! -e "$S/deleted" ]' "rc=$rc"

# --- usage
fresh; PATH="$t/bin:$PATH" STUB_STATE=$S GITHUB_REPOSITORY=o/r "$dr" publish >/dev/null 2>&1; rc=$?
check "usage: a missing tag fails" '[ "$rc" = 2 ]' "rc=$rc"
fresh; PATH="$t/bin:$PATH" STUB_STATE=$S GITHUB_REPOSITORY=o/r "$dr" frobnicate v1.2.3 >/dev/null 2>&1; rc=$?
check "usage: an unknown mode fails" '[ "$rc" = 2 ]' "rc=$rc"

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
