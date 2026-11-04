#!/bin/bash
# Tests for .github/scripts/next-version.sh and check-versions.sh.
#   next-version.sh <current-version>...   commit messages on stdin; prints the next X.Y.Z
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
want 0.3.4 "docs and chore stay patch" $'docs: x\nchore: y\nrefactor(foo): z' 0.3.3 0.3.3 0.3.3
want 0.4.0 "first run: lockstep broken, highest 0.3.3 -> 0.4.0" "fix: a bug" 0.3.3 0.1.0 0.1.0
want 0.4.0 "first run with a feat" "feat: x" 0.3.3 0.1.0 0.1.0
want 0.4.0 "feat: is minor" "feat: new thing" 0.3.3 0.3.3 0.3.3
want 0.4.0 "feat(scope): is minor" "feat(memory): new thing" 0.3.3 0.3.3 0.3.3
want 0.4.0 "feat in a merge commit body" $'Merge pull request #21 from a/b\n\nfeat(statusline): thing' 0.3.3 0.3.3 0.3.3
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
