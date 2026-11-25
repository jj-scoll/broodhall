#!/bin/sh
# coverage-badge.sh < `go tool cover -func=cover.out` output
# Prints a shields.io endpoint badge (https://shields.io/badges/endpoint-badge) for the total
# statement coverage. With no readable total it prints nothing and exits 1, so the caller
# publishes no badge rather than a wrong one.
set -eu
pct=$(awk '$1 == "total:" { sub(/%$/, "", $NF); print $NF }')
case $pct in
'' | *[!0-9.]* | .* | *.*.*) echo "coverage-badge.sh: no total coverage in the input" >&2; exit 1 ;;
esac
color=$(awk -v p="$pct" 'BEGIN {
	if (p >= 90) c = "brightgreen"; else if (p >= 80) c = "green"; else if (p >= 70) c = "yellowgreen"
	else if (p >= 60) c = "yellow"; else if (p >= 50) c = "orange"; else c = "red"
	print c }')
printf '{"schemaVersion":1,"label":"coverage","message":"%s%%","color":"%s"}\n' "$pct" "$color"
