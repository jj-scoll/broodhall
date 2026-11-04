#!/bin/bash
# Tests for .github/scripts/pack-memory.sh <dir>: each memory_<os>_<arch>.tar becomes a .tar.xz
# that `tar -xJf` (what memory's run.sh runs) unpacks to the same files. amd64 gets the x86 BCJ
# filter (decodable by every xz since 5.0); arm64 gets plain LZMA2, because the arm64 filter
# needs xz >= 5.4 and Ubuntu 22.04 / RHEL 9 ship 5.2.5.
here=$(cd "$(dirname "$0")/.." && pwd)
pm=$here/pack-memory.sh
pass=0 fail=0
ok() { pass=$((pass + 1)); echo "ok   $1"; }
bad() { fail=$((fail + 1)); echo "FAIL $1: $2"; }

t=$(mktemp -d)
trap 'rm -rf "$t"' EXIT

# A tar shaped like a release one: an executable "memory" and ext/.
mkt() { # mkt <dir> <name>
	w=$t/src/${1##*/}/$2
	mkdir -p "$w/ext"
	cp "$(command -v sh)" "$w/memory"
	head -c 65536 /dev/urandom >"$w/ext/fts.duckdb_extension"
	tar -cf "$1/$2.tar" -C "$w" memory ext
}
# chain <file.xz>: the filter chain of its first block, e.g. "--x86 --lzma2=dict=8MiB"
chain() { xz --robot --list -vv "$1" | awk -F'\t' '$1 == "block" { print $NF; exit }'; }

# --- 1. four platforms: outputs, filters, round trip
d=$t/dist
mkdir -p "$d"
plats="linux_amd64 linux_arm64 darwin_amd64 darwin_arm64"
for p in $plats; do mkt "$d" "memory_$p"; done
printf 'gz' >"$d/claude-tools_linux_amd64.tar.gz"
if "$pm" "$d" >"$t/out1" 2>&1; then ok "packs four platforms"; else bad "packs four platforms" "exit $?: $(cat "$t/out1")"; fi
for p in $plats; do
	f=$d/memory_$p.tar.xz
	if [ -f "$f" ] && [ ! -e "$d/memory_$p.tar" ]; then ok "$p: .tar replaced by .tar.xz"; else bad "$p: .tar replaced by .tar.xz" "$(ls "$d")"; fi
	c=$(chain "$f")
	case $p in
	*amd64) want="--x86 --lzma2=" ;;
	*) want="--lzma2=" ;;
	esac
	case $c in
	"$want"*) ok "$p: filter chain $c" ;;
	*) bad "$p: filter chain" "want $want..., got '$c'" ;;
	esac
	u=$t/unpack/$p
	mkdir -p "$u"
	s=$t/src/dist/memory_$p
	if tar -xJf "$f" -C "$u" memory ext && cmp -s "$u/memory" "$s/memory" &&
		cmp -s "$u/ext/fts.duckdb_extension" "$s/ext/fts.duckdb_extension" && [ -x "$u/memory" ]; then
		ok "$p: tar -xJf round trip"
	else
		bad "$p: tar -xJf round trip" "contents differ or unpack failed"
	fi
done
if [ "$(cat "$d/claude-tools_linux_amd64.tar.gz")" = gz ]; then ok "claude-tools .tar.gz untouched"; else bad "claude-tools .tar.gz untouched" "changed"; fi

# --- 2. one failing input fails the whole run
d=$t/dist2
mkdir -p "$d"
mkt "$d" memory_linux_amd64
ln -s "$t/nope" "$d/memory_linux_arm64.tar"
if "$pm" "$d" >/dev/null 2>&1; then bad "a failing input fails the run" "exited 0"; else ok "a failing input fails the run"; fi

# --- 3. no memory tars is an error, not a silent pass
mkdir -p "$t/empty"
if "$pm" "$t/empty" >/dev/null 2>&1; then bad "no tars fails" "exited 0"; else ok "no tars fails"; fi

# --- 4. usage
if "$pm" >/dev/null 2>&1; then bad "no argument fails" "exited 0"; else ok "no argument fails"; fi

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
