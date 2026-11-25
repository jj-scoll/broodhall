#!/bin/bash
# Tests for .github/scripts/coverage-badge.sh: `go tool cover -func` output on stdin, a shields.io
# endpoint JSON on stdout.
here=$(cd "$(dirname "$0")/.." && pwd)
cb=$here/coverage-badge.sh
pass=0 fail=0

# want <name> <expected stdout> <stdin>
want() {
	got=$(printf '%s\n' "$3" | "$cb" 2>&1)
	if [ "$got" = "$2" ]; then pass=$((pass + 1)); echo "ok   $1"; else fail=$((fail + 1)); echo "FAIL $1: want '$2', got '$got'"; fi
}
# wantfail <name> <stdin>: exits nonzero and prints nothing on stdout
wantfail() {
	if got=$(printf '%s\n' "$2" | "$cb" 2>/dev/null); then fail=$((fail + 1)); echo "FAIL $1: exited 0 with '$got'"
	elif [ -n "$got" ]; then fail=$((fail + 1)); echo "FAIL $1: printed '$got'"
	else pass=$((pass + 1)); echo "ok   $1"; fi
}
func() { printf 'github.com/x/y/a.go:10:\tf\t\t100.0%%\ntotal:\t\t\t\t(statements)\t%s\n' "$1"; }
j() { printf '{"schemaVersion":1,"label":"coverage","message":"%s","color":"%s"}' "$1" "$2"; }

want "brightgreen at 90" "$(j 90.0% brightgreen)" "$(func 90.0%)"
want "green below 90" "$(j 89.9% green)" "$(func 89.9%)"
want "green at 80" "$(j 80.0% green)" "$(func 80.0%)"
want "yellowgreen below 80" "$(j 79.9% yellowgreen)" "$(func 79.9%)"
want "yellow at 60" "$(j 60.0% yellow)" "$(func 60.0%)"
want "orange at 50" "$(j 50.0% orange)" "$(func 50.0%)"
want "red below 50" "$(j 49.9% red)" "$(func 49.9%)"
want "100%" "$(j 100.0% brightgreen)" "$(func 100.0%)"
want "0%" "$(j 0.0% red)" "$(func 0.0%)"
wantfail "no total line" 'github.com/x/y/a.go:10:	f		100.0%'
wantfail "empty input" ''
wantfail "total not a percentage" "$(func n/a)"

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
