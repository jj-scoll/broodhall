#!/bin/bash
# Fail-open tests for plugins/memory/scripts/run.sh. No dependencies beyond sh, tar, curl,
# sha256sum or shasum. Run: bash plugins/memory/test/run_test.sh
#
# Contract under test: whatever goes wrong (no binary, failed download, checksum mismatch,
# unsupported platform, crashing binary, hung binary, daemon down), the launcher exits 0 and
# writes nothing to stdout, and an unverified binary is never executed.
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
asset=memory_${os}_$arch.tar.gz

# A stub binary. STUB_MODE picks the behaviour; STUB_MARK records every execution.
mkstub() {
	cat >"$1" <<'EOF'
#!/bin/sh
[ -n "${STUB_MARK-}" ] && echo "$*" >>"$STUB_MARK"
case "${STUB_MODE-ok}" in
ok)
	case "$1" in
	context) cat >/dev/null; echo '{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"stub memory"}}' ;;
	serve) exit 0 ;;
	*) cat >/dev/null ;;
	esac ;;
crash) echo '{"decision":"block","reason":"partial"}'; echo "boom" >&2; exit 3 ;;
hang) echo $$ >"$STUB_PIDFILE"; exec sleep 30 ;;
daemon-down) cat >/dev/null; echo "memory: cannot reach daemon" >&2; exit 1 ;;
serve-hangs) case "$1" in serve) exec sleep 30 ;; context) cat >/dev/null; echo '{"ctx":1}' ;; esac ;;
esac
EOF
	chmod +x "$1"
}

# A fake release dir for tag memory-v$ver with a good tarball and matching checksums.
mkrelease() {
	rel=$1
	mkdir -p "$rel/memory-v$ver" "$rel/work"
	mkstub "$rel/work/memory"
	tar -czf "$rel/memory-v$ver/$asset" -C "$rel/work" memory
	(cd "$rel/memory-v$ver" && $sum "$asset" >checksums.txt)
}

# case <name>: fresh sandbox dirs and a clean environment.
newcase() {
	c=$tmp/$1
	mkdir -p "$c/data" "$c/path"
	: >"$c/mark"
	unset MEMORY_BIN STUB_MODE
	export CLAUDE_PLUGIN_ROOT=$root CLAUDE_PLUGIN_DATA=$c/data HOME=$c/home
	export STUB_MARK=$c/mark STUB_PIDFILE=$c/pid MEMORY_HOOK_TIMEOUT=2
	export MEMORY_RELEASE_URL=file://$c/no-such-release
}

# hook <case> <args...>: run the launcher the way Claude Code does, capture stdout/rc/seconds.
hook() {
	c=$tmp/$1
	shift
	local t0 t1
	t0=$(date +%s)
	out=$(printf '{"cwd":"/tmp","source":"startup"}' | sh "$run_sh" "$@" 2>"$c/stderr")
	rc=$?
	t1=$(date +%s)
	secs=$((t1 - t0))
}

expect_open() { # <name> : exit 0 and empty stdout
	if [ "$rc" -ne 0 ]; then
		bad "$1" "exit $rc, want 0"
	elif [ -n "$out" ]; then
		bad "$1" "stdout not empty: $out"
	else
		ok "$1"
	fi
}

# --- 1. binary missing, download unavailable
newcase t1
hook t1 session-start
expect_open "binary missing and download fails (session-start)"
hook t1 context
expect_open "binary missing (context)"
hook t1 mcp
expect_open "binary missing (mcp)"

# --- 2. positive control: download, verify, install, then context passes through
newcase t2
mkrelease "$tmp/t2/rel"
export MEMORY_RELEASE_URL=file://$tmp/t2/rel
hook t2 session-start
if [ "$rc" -eq 0 ] && [ -x "$tmp/t2/data/bin/$ver/memory" ] && printf %s "$out" | grep -q 'stub memory'; then
	ok "good release installs and context reaches stdout"
else
	bad "good release installs and context reaches stdout" "rc=$rc installed=$([ -x "$tmp/t2/data/bin/$ver/memory" ] && echo yes || echo no) out=$out"
fi

# --- 3. checksum mismatch: never execute, never install
newcase t3
mkrelease "$tmp/t3/rel"
echo "0000000000000000000000000000000000000000000000000000000000000000  $asset" >"$tmp/t3/rel/memory-v$ver/checksums.txt"
export MEMORY_RELEASE_URL=file://$tmp/t3/rel
hook t3 session-start
expect_open "checksum mismatch (session-start)"
if [ -e "$tmp/t3/data/bin/$ver/memory" ]; then bad "checksum mismatch installs nothing" "binary present"; else ok "checksum mismatch installs nothing"; fi
if [ -s "$tmp/t3/mark" ]; then bad "checksum mismatch never executes the binary" "stub ran: $(cat "$tmp/t3/mark")"; else ok "checksum mismatch never executes the binary"; fi
hook t3 context
expect_open "checksum mismatch (context after failed install)"

# --- 3b. checksums.txt has no line for this asset
newcase t3b
mkrelease "$tmp/t3b/rel"
echo "deadbeef  some_other_asset.tar.gz" >"$tmp/t3b/rel/memory-v$ver/checksums.txt"
export MEMORY_RELEASE_URL=file://$tmp/t3b/rel
hook t3b session-start
expect_open "checksums.txt lacks the asset"
if [ -s "$tmp/t3b/mark" ]; then bad "unlisted asset never executes" "stub ran"; else ok "unlisted asset never executes"; fi

# --- 4. unsupported platform: stub uname, and curl must not be called
newcase t4
mkdir -p "$tmp/t4/path"
cat >"$tmp/t4/path/uname" <<'EOF'
#!/bin/sh
case "$1" in -s) echo Plan9 ;; -m) echo riscv64 ;; *) echo Plan9 ;; esac
EOF
cat >"$tmp/t4/path/curl" <<EOF
#!/bin/sh
echo curl >>"$tmp/t4/curl-called"
exit 1
EOF
chmod +x "$tmp/t4/path/uname" "$tmp/t4/path/curl"
old_path=$PATH
PATH=$tmp/t4/path:$PATH
hook t4 session-start
PATH=$old_path
expect_open "unsupported platform"
if [ -e "$tmp/t4/curl-called" ]; then bad "unsupported platform skips the download" "curl was called"; else ok "unsupported platform skips the download"; fi

# --- 5. binary crashes after printing a block decision
newcase t5
mkstub "$tmp/t5/memory"
export MEMORY_BIN=$tmp/t5/memory STUB_MODE=crash
hook t5 context
expect_open "binary crashes with partial block JSON on stdout (context)"
hook t5 session-start
expect_open "binary crashes (session-start)"

# --- 6. binary hangs: killed at the timeout, no stdout, process gone
newcase t6
mkstub "$tmp/t6/memory"
export MEMORY_BIN=$tmp/t6/memory STUB_MODE=hang MEMORY_HOOK_TIMEOUT=1
hook t6 context
expect_open "binary hangs (context)"
if [ "$secs" -le 4 ]; then ok "hang returns within the timeout (${secs}s)"; else bad "hang returns within the timeout" "took ${secs}s"; fi
sleep 1
if [ -s "$tmp/t6/pid" ] && kill -0 "$(cat "$tmp/t6/pid")" 2>/dev/null; then
	bad "hung binary is killed" "pid $(cat "$tmp/t6/pid") still alive"
	kill -9 "$(cat "$tmp/t6/pid")" 2>/dev/null
else
	ok "hung binary is killed"
fi

# --- 7. daemon down: context client exits 1
newcase t7
mkstub "$tmp/t7/memory"
export MEMORY_BIN=$tmp/t7/memory STUB_MODE=daemon-down
hook t7 context
expect_open "daemon down (context exits 1)"
hook t7 session-start
expect_open "daemon down (session-start)"

# --- 8. the daemon spawn must not block the hook, and must not leak onto stdout
newcase t8
mkstub "$tmp/t8/memory"
export MEMORY_BIN=$tmp/t8/memory STUB_MODE=serve-hangs MEMORY_HOOK_TIMEOUT=2
hook t8 session-start
if [ "$rc" -eq 0 ] && [ "$secs" -le 4 ] && [ "$out" = '{"ctx":1}' ]; then
	ok "a long-running serve is detached and does not block session-start (${secs}s)"
else
	bad "a long-running serve is detached and does not block session-start" "rc=$rc secs=$secs out=$out"
fi
pkill -f "sleep 30" 2>/dev/null

# --- 9. data directory not writable
newcase t9
mkdir -p "$tmp/t9/ro"
chmod 500 "$tmp/t9/ro"
export CLAUDE_PLUGIN_DATA=$tmp/t9/ro/nested
mkrelease "$tmp/t9/rel"
export MEMORY_RELEASE_URL=file://$tmp/t9/rel
hook t9 session-start
expect_open "data dir not writable"
chmod 700 "$tmp/t9/ro"

# --- 10. diagnostics go to a log or stderr, never stdout (covered above); a failure leaves a trace
newcase t10
hook t10 session-start
if [ -s "$tmp/t10/stderr" ] || [ -s "$tmp/t10/data/memory.log" ]; then ok "failure leaves a diagnostic in stderr or the log"; else bad "failure leaves a diagnostic in stderr or the log" "nothing written"; fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
