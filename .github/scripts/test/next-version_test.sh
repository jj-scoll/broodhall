#!/bin/bash
# Tests for .github/scripts/next-version.sh and check-versions.sh.
#   next-version.sh <current-version>...   commit messages on stdin; prints the next X.Y.Z
# Commits on stdin are separated by RS (0x1e), as `git log --format=%x1e%B` writes them; the first
# line of each is its title. Test logs without an RS are a single commit.
RS=$'\x1e'
here=$(cd "$(dirname "$0")/.." && pwd)
nv=$here/next-version.sh
cv=$here/check-versions.sh
pass=0 fail=0

# want <expected> <name> <log> <version>...
want() {
	exp=$1 name=$2 log=$3
	shift 3
	got=$(printf '%s\n' "$log" | "$nv" "$@" 2>&1)
	if [ "$got" = "$exp" ]; then pass=$((pass + 1)); echo "ok   $name"; else fail=$((fail + 1)); echo "FAIL $name: want '$exp', got '$got'"; fi
}
# wantfail <name> <log> <version>...: must exit nonzero
wantfail() {
	name=$1 log=$2
	shift 2
	if printf '%s\n' "$log" | "$nv" "$@" >/dev/null 2>&1; then fail=$((fail + 1)); echo "FAIL $name: exited 0"; else pass=$((pass + 1)); echo "ok   $name"; fi
}

want 0.3.4 "patch by default" "fix: a bug" 0.3.3 0.3.3 0.3.3
want 0.3.4 "docs and chore stay patch" "docs: x${RS}chore: y${RS}refactor(foo): z" 0.3.3 0.3.3 0.3.3
want 0.4.0 "first run: lockstep broken, highest 0.3.3 -> 0.4.0" "fix: a bug" 0.3.3 0.1.0 0.1.0
want 0.4.0 "first run with a feat" "feat: x" 0.3.3 0.1.0 0.1.0
want 0.4.0 "feat: is minor" "feat: new thing" 0.3.3 0.3.3 0.3.3
want 0.4.0 "feat(scope): is minor" "feat(memory): new thing" 0.3.3 0.3.3 0.3.3
want 0.3.4 "a merge commit's body (the PR title) does not count as a title" $'Merge pull request #21 from a/b\n\nfeat(statusline): thing' 0.3.3 0.3.3 0.3.3
want 0.4.0 "#minor anywhere" "fix: small but #minor" 0.3.3 0.3.3 0.3.3
want 1.0.0 "BREAKING CHANGE is major" $'feat: x\n\nBREAKING CHANGE: gone' 0.3.3 0.3.3 0.3.3
want 1.0.0 "#major" "fix: y #major" 0.3.3 0.3.3 0.3.3
want 1.0.0 "type! is major" "feat!: drop it" 0.3.3 0.3.3 0.3.3
want 1.0.0 "major beats the first-run minor" "feat: x #major" 0.3.3 0.1.0 0.1.0
want 2.0.0 "major resets minor and patch" "#major" 1.4.7 1.4.7 1.4.7
want 1.5.0 "minor resets patch" "feat: x" 1.4.7 1.4.7 1.4.7
want 0.3.11 "numeric, not lexical, patch" "fix: x" 0.3.10
want 0.11.0 "numeric highest (0.9.0 vs 0.10.0), mismatch -> minor" "fix: x" 0.9.0 0.10.0 0.10.0
want 0.4.0 "ordering of args does not matter" "fix: x" 0.1.0 0.3.3 0.1.0
want 0.3.4 "text containing 'feat' mid-line is not a feat title" "fix: handle feat: parsing" 0.3.3
want 0.3.4 "empty log" "" 0.3.3
# --- rules apply to the title, or to a footer, never to prose in a body
body=$'feat: autorelease\n\n- next-version.sh: patch by default, minor on\n  feat / #minor / broken lockstep, major on BREAKING CHANGE / type! / #major\n- this commit is a feat, so the first automatic release is 0.4.0'
want 0.4.0 "regression: the body text that cut 1.0.0 is only a feat" "$body" 0.3.3 0.3.3 0.3.3
want 0.3.4 "#major in a body is ignored" $'fix: x\n\nmajor on #major' 0.3.3 0.3.3 0.3.3
want 0.3.4 "#minor in a body is ignored" $'fix: x\n\nminor on #minor' 0.3.3 0.3.3 0.3.3
want 0.3.4 "type!: at the start of a body line is ignored" $'fix: x\n\nfeat!: not a title' 0.3.3 0.3.3 0.3.3
want 0.3.4 "type(scope)!: at the start of a body line is ignored" $'fix: x\n\nfeat(a)!: not a title' 0.3.3 0.3.3 0.3.3
want 0.3.4 "feat: at the start of a body line is ignored" $'fix: x\n\nfeat: not a title' 0.3.3 0.3.3 0.3.3
want 0.3.4 "feat(scope): at the start of a body line is ignored" $'fix: x\n\nfeat(a): not a title' 0.3.3 0.3.3 0.3.3
want 1.0.0 "type(scope)!: title is major" "refactor(api)!: drop it" 0.3.3 0.3.3 0.3.3
want 1.0.0 "BREAKING CHANGE: footer is major" $'fix: x\n\nBREAKING CHANGE: gone' 0.3.3 0.3.3 0.3.3
want 1.0.0 "BREAKING-CHANGE: footer is major" $'fix: x\n\nsome text\n\nBREAKING-CHANGE: gone' 0.3.3 0.3.3 0.3.3
want 0.3.4 "BREAKING CHANGE mid-line is ignored" $'fix: x\n\nwe avoid a BREAKING CHANGE: here' 0.3.3 0.3.3 0.3.3
want 0.3.4 "BREAKING CHANGE without a colon is ignored" $'fix: x\n\nBREAKING CHANGE on the line' 0.3.3 0.3.3 0.3.3
want 0.3.4 "BREAKING CHANGE: without a space is ignored" $'fix: x\n\nBREAKING CHANGE:gone' 0.3.3 0.3.3 0.3.3
want 0.3.4 "BREAKING CHANGE: as a title is ignored" "BREAKING CHANGE: gone" 0.3.3 0.3.3 0.3.3
mrg=$'Merge pull request #21 from jj-scoll/fix-#major\n\nfix: thing #minor'
want 0.3.4 "regression: a merge commit's title and body do not count" "$mrg" 0.3.3 0.3.3 0.3.3
want 0.4.0 "a merge commit is skipped but the PR's own commit is seen" "${mrg}${RS}feat: the real change" 0.3.3 0.3.3 0.3.3
want 0.4.0 "a leading RS (git log format) is harmless" "${RS}feat: x${RS}fix: y" 0.3.3 0.3.3 0.3.3
wantfail "no versions" "fix: x"
wantfail "invalid semver" "fix: x" 0.3 0.3.3
wantfail "prerelease rejected" "fix: x" 0.3.3-rc1

# check-versions.sh <plugin.json>...: all equal, valid semver
mk() { printf '{\n  "name": "n",\n  "version": "%s"\n}\n' "$2" >"$1"; }
t=$(mktemp -d)
trap 'rm -rf "$t"' EXIT
mk "$t/a.json" 0.4.0
mk "$t/b.json" 0.4.0
mk "$t/c.json" 0.4.1
mk "$t/d.json" v1
if "$cv" "$t/a.json" "$t/b.json" >/dev/null 2>&1; then pass=$((pass + 1)); echo "ok   check-versions: equal passes"; else fail=$((fail + 1)); echo "FAIL check-versions: equal rejected"; fi
if "$cv" "$t/a.json" "$t/c.json" >/dev/null 2>&1; then fail=$((fail + 1)); echo "FAIL check-versions: mismatch accepted"; else pass=$((pass + 1)); echo "ok   check-versions: mismatch fails"; fi
if "$cv" "$t/d.json" "$t/d.json" >/dev/null 2>&1; then fail=$((fail + 1)); echo "FAIL check-versions: invalid semver accepted"; else pass=$((pass + 1)); echo "ok   check-versions: invalid semver fails"; fi

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
