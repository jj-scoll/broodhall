#!/bin/bash
# Tests for .github/scripts/pin-checksums.sh, which carries a release's sha256 list into the
# plugin directories (plugins/<plugin>/checksums.txt), where the launchers read it from the git
# checkout. The last case installs with the real launchers from the files it wrote.
# Run: bash .github/scripts/test/pin-checksums_test.sh
here=$(cd "$(dirname "$0")/.." && pwd)
repo=$(cd "$here/../.." && pwd)
pc=$here/pin-checksums.sh
pass=0 fail=0
ok() { pass=$((pass + 1)); echo "ok   $1"; }
bad() { fail=$((fail + 1)); echo "FAIL $1: $2"; }

t=$(mktemp -d)
trap 'rm -rf "$t"' EXIT
if command -v sha256sum >/dev/null; then sum=sha256sum; else sum='shasum -a 256'; fi

# A release's dist/: eight archives (the two for this machine hold a runnable stub) and the
# checksums.txt the publish job writes.
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case $(uname -m) in x86_64 | amd64) arch=amd64 ;; *) arch=arm64 ;; esac
d=$t/rel/v9.9.9
mkdir -p "$d" "$t/work/ext"
printf '#!/bin/sh\nexit 0\n' >"$t/work/guardrails"
cp "$t/work/guardrails" "$t/work/memory"
chmod +x "$t/work/guardrails" "$t/work/memory"
for e in fts httpfs quack; do echo "$e" >"$t/work/ext/$e.duckdb_extension"; done
for p in linux_amd64 linux_arm64 darwin_amd64 darwin_arm64; do
	echo "guardrails $p" >"$d/guardrails_$p.tar.gz"
	echo "memory $p" >"$d/memory_$p.tar.xz"
done
tar -czf "$d/guardrails_${os}_$arch.tar.gz" -C "$t/work" guardrails
tar -cJf "$d/memory_${os}_$arch.tar.xz" -C "$t/work" memory ext
(cd "$d" && $sum -- *.tar.gz *.tar.xz >checksums.txt)

# mkroot <dir>: a tree with the two plugin directories, as the repository has them
mkroot() {
	for p in guardrails memory; do
		mkdir -p "$1/plugins/$p"
		cp -R "$repo/plugins/$p/scripts" "$repo/plugins/$p/.claude-plugin" "$1/plugins/$p/"
		sed -i.bak -E 's/"version": *"[^"]*"/"version": "9.9.9"/' "$1/plugins/$p/.claude-plugin/plugin.json"
	done
}

# --- 1. write: each plugin gets its version line and its own four archives, nothing else
r=$t/r1
mkroot "$r"
if "$pc" write "$d/checksums.txt" 9.9.9 "$r" >"$t/out1" 2>&1; then ok "write succeeds"; else bad "write succeeds" "exit $?: $(cat "$t/out1")"; fi
for p in guardrails memory; do
	f=$r/plugins/$p/checksums.txt
	if grep -qxF "# version 9.9.9" "$f" 2>/dev/null; then ok "$p: names the version"; else bad "$p: names the version" "$(cat "$f" 2>&1)"; fi
	n=$(grep -cE "^[0-9a-f]{64}  ${p}_(linux|darwin)_(amd64|arm64)\.tar\.(gz|xz)$" "$f" 2>/dev/null)
	if [ "$n" = 4 ] && [ "$(grep -vc '^#' "$f")" = 4 ]; then ok "$p: lists its four archives only"; else bad "$p: lists its four archives only" "$(cat "$f" 2>&1)"; fi
	if grep -v '^#' "$f" | (cd "$d" && $sum -c - >/dev/null 2>&1); then ok "$p: the sums are the archives' own"; else bad "$p: the sums are the archives' own" "$(cat "$f")"; fi
done

# --- 2. write refuses a list that lacks an archive, a malformed sum and a malformed version,
# and leaves no file behind
r=$t/r2
mkroot "$r"
grep -v 'memory_linux_arm64' "$d/checksums.txt" >"$t/short.txt"
if "$pc" write "$t/short.txt" 9.9.9 "$r" >/dev/null 2>&1; then bad "a list short of an archive fails" "exited 0"; else ok "a list short of an archive fails"; fi
sed 's/^[0-9a-f]\{8\}/zzzzzzzz/' "$d/checksums.txt" >"$t/malformed.txt"
if "$pc" write "$t/malformed.txt" 9.9.9 "$r" >/dev/null 2>&1; then bad "a malformed sum fails" "exited 0"; else ok "a malformed sum fails"; fi
if "$pc" write "$d/checksums.txt" v9.9.9 "$r" >/dev/null 2>&1; then bad "a version that is not X.Y.Z fails" "exited 0"; else ok "a version that is not X.Y.Z fails"; fi
if [ -z "$(find "$r" -name checksums.txt)" ]; then ok "a failed write leaves no checksums.txt"; else bad "a failed write leaves no checksums.txt" "$(find "$r" -name checksums.txt)"; fi
if "$pc" write "$d/checksums.txt" >/dev/null 2>&1; then bad "write without a version fails" "exited 0"; else ok "write without a version fails"; fi
if "$pc" >/dev/null 2>&1; then bad "no argument fails" "exited 0"; else ok "no argument fails"; fi

# --- 3. check: a tree without the files passes (a release being built); one whose files match
# the built list passes; one whose files differ fails (a rebuild of a tag that pins other bytes)
r=$t/r3
mkroot "$r"
if "$pc" check "$d/checksums.txt" "$r" >/dev/null 2>&1; then ok "check passes with no pinned files"; else bad "check passes with no pinned files" "exit $?"; fi
if "$pc" check "$d/checksums.txt" "$t/r1" >/dev/null 2>&1; then ok "check passes when the pinned files match"; else bad "check passes when the pinned files match" "exit $?"; fi
sed '/memory_linux_arm64/s/^[0-9a-f]\{8\}/00000000/' "$d/checksums.txt" >"$t/rebuilt.txt"
if out=$("$pc" check "$t/rebuilt.txt" "$t/r1" 2>&1); then bad "check fails when an archive differs from its pin" "exited 0"; else
	case $out in *memory_linux_arm64.tar.xz*) ok "check fails when an archive differs from its pin, and names it" ;; *) bad "check names the archive" "$out" ;; esac
fi

# --- 4. the launchers install from the files write made, with no checksums.txt in the release
rm "$d/checksums.txt"
for p in guardrails memory; do
	data=$t/data-$p
	out=$(CLAUDE_PLUGIN_ROOT=$t/r1/plugins/$p CLAUDE_PLUGIN_DATA=$data HOME=$t/home \
		GUARDRAILS_RELEASE_URL=file://$t/rel MEMORY_RELEASE_URL=file://$t/rel MEMORY_NO_DAEMON=1 \
		sh "$t/r1/plugins/$p/scripts/run.sh" install </dev/null 2>&1)
	if [ -x "$data/bin/9.9.9/$p" ]; then ok "$p: its launcher installs from the written file"; else bad "$p: its launcher installs from the written file" "out=$out log=$(cat "$data/memory.log" 2>/dev/null)"; fi
done

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
