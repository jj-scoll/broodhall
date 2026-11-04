#!/bin/bash
# Tests for the install path of plugins/guardrails/scripts/run.sh: what `run.sh install` does
# with the doctor. Needs sh, tar, curl, sha256sum or shasum. Run: bash plugins/guardrails/test/run_test.sh
#
# Contract under test: install records the unpacked binary's sha256 beside it, then runs
# `claude-tools doctor -hook` and passes its output through; install always exits 0, whether
# doctor succeeds, crashes, or the download fails; a binary that failed its checksum is
# never run.
set -u
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/.." && pwd)
run_sh=$root/scripts/run.sh
ver=$(sed -n 's/^ *"version": *"\([^"]*\)".*/\1/p' "$root/.claude-plugin/plugin.json")

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
pass=0 fail=0
ok() { pass=$((pass + 1)); printf 'ok   %s\n' "$1"; }
bad() { fail=$((fail + 1)); printf 'FAIL %s: %s\n' "$1" "$2"; }

if command -v sha256sum >/dev/null; then sum=sha256sum; else sum='shasum -a 256'; fi
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case $(uname -m) in x86_64 | amd64) arch=amd64 ;; *) arch=arm64 ;; esac
asset=claude-tools_${os}_$arch.tar.gz

# A stub binary: records its arguments and CLAUDE_PLUGIN_ROOT, and answers `doctor -hook`
# according to STUB_MODE (ok: one problem line, crash: exit 3 with partial output).
mkstub() {
	cat >"$1" <<'EOF'
#!/bin/sh
echo "$* root=${CLAUDE_PLUGIN_ROOT-}" >>"$STUB_MARK"
cat >/dev/null
case "$1" in
doctor)
	case "${STUB_MODE-ok}" in
	crash) echo '{"partial":'; exit 3 ;;
	*) echo '{"systemMessage":"doctor says hi"}' ;;
	esac ;;
esac
EOF
	chmod +x "$1"
}

# A fake release for tag v$ver. BAD_SUM=1 ships a checksums.txt that does not match.
mkrelease() {
	rel=$1
	mkdir -p "$rel/v$ver" "$rel/work"
	mkstub "$rel/work/claude-tools"
	tar -czf "$rel/v$ver/$asset" -C "$rel/work" claude-tools
	(cd "$rel/v$ver" && $sum "$asset" >checksums.txt)
	[ -n "${BAD_SUM-}" ] && sed -i.bak 's/^./0/' "$rel/v$ver/checksums.txt"
	return 0
}

newcase() {
	c=$tmp/$1
	mkdir -p "$c/data" "$c/home"
	: >"$c/mark"
	unset CLAUDE_TOOLS_BIN STUB_MODE BAD_SUM
	export CLAUDE_PLUGIN_ROOT=$root CLAUDE_PLUGIN_DATA=$c/data HOME=$c/home STUB_MARK=$c/mark
	export CLAUDE_TOOLS_RELEASE_URL=file://$c/no-such-release
}

install() { # <case>
	c=$tmp/$1
	out=$(printf '{"source":"startup"}' | sh "$run_sh" install 2>"$c/stderr")
	rc=$?
}

bin() { printf %s "$tmp/$1/data/bin/$ver/claude-tools"; }

# --- 1. fresh install: binary in place, checksum recorded, doctor run and passed through
newcase t1
mkrelease "$tmp/t1/rel"
export CLAUDE_TOOLS_RELEASE_URL=file://$tmp/t1/rel
install t1
[ "$rc" -eq 0 ] && [ -x "$(bin t1)" ] && ok "fresh install puts the binary in place" || bad "fresh install puts the binary in place" "rc=$rc $(cat "$tmp/t1/stderr")"
want=$($sum "$(bin t1)" | sed 's/ .*//')
got=$(sed 's/ .*//' "$(bin t1).sha256" 2>/dev/null)
[ -n "$want" ] && [ "$want" = "$got" ] && ok "install records the unpacked binary's sha256" || bad "install records the unpacked binary's sha256" "want=$want got=$got"
grep -q '  claude-tools$' "$(bin t1).sha256" && ok "the record is in checksums.txt format" || bad "the record is in checksums.txt format" "$(cat "$(bin t1).sha256")"
printf %s "$out" | grep -q 'doctor says hi' && ok "doctor's reply passes through install" || bad "doctor's reply passes through install" "out=$out"
grep -q "^doctor -hook root=$root\$" "$tmp/t1/mark" && ok "doctor runs with -hook and CLAUDE_PLUGIN_ROOT set" || bad "doctor runs with -hook and CLAUDE_PLUGIN_ROOT set" "$(cat "$tmp/t1/mark")"
[ -z "$(ls -A "$tmp/t1/data/bin" | grep '^\.download')" ] && ok "install removes its download directory" || bad "install removes its download directory" "$(ls -A "$tmp/t1/data/bin")"

# --- 2. already installed: no download, doctor still runs
export CLAUDE_TOOLS_RELEASE_URL=file://$tmp/t1/no-such-release
: >"$tmp/t1/mark"
install t1
if [ "$rc" -eq 0 ] && grep -q '^doctor -hook' "$tmp/t1/mark"; then ok "an installed binary skips the download and runs doctor"; else bad "an installed binary skips the download and runs doctor" "rc=$rc mark=$(cat "$tmp/t1/mark")"; fi

# --- 3. a crashing doctor never fails install
newcase t3
mkrelease "$tmp/t3/rel"
export CLAUDE_TOOLS_RELEASE_URL=file://$tmp/t3/rel STUB_MODE=crash
install t3
[ "$rc" -eq 0 ] && ok "install exits 0 when doctor crashes (fresh)" || bad "install exits 0 when doctor crashes (fresh)" "rc=$rc"
install t3
[ "$rc" -eq 0 ] && ok "install exits 0 when doctor crashes (already installed)" || bad "install exits 0 when doctor crashes (already installed)" "rc=$rc"

# --- 4. a checksum mismatch installs nothing and runs nothing
newcase t4
BAD_SUM=1 mkrelease "$tmp/t4/rel"
export CLAUDE_TOOLS_RELEASE_URL=file://$tmp/t4/rel
install t4
if [ "$rc" -eq 0 ] && [ ! -e "$(bin t4)" ] && [ ! -s "$tmp/t4/mark" ] && printf %s "$out" | grep -q 'checksum mismatch'; then
	ok "a checksum mismatch installs nothing, runs nothing and says so"
else
	bad "a checksum mismatch installs nothing, runs nothing and says so" "rc=$rc out=$out mark=$(cat "$tmp/t4/mark")"
fi

# --- 5. download unavailable: message, exit 0, nothing run
newcase t5
install t5
if [ "$rc" -eq 0 ] && [ ! -s "$tmp/t5/mark" ] && printf %s "$out" | grep -q 'could not install'; then ok "a failed download is reported and exits 0"; else bad "a failed download is reported and exits 0" "rc=$rc out=$out"; fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
