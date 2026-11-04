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
asset=memory_${os}_$arch.tar.xz

# A stub binary. STUB_MODE picks the behaviour; STUB_MARK records every execution.
mkstub() {
	cat >"$1" <<'EOF'
#!/bin/sh
[ -n "${STUB_MARK-}" ] && echo "$* dir=${MEMORY_DIR-} ext=${MEMORY_EXT_DIR-}" >>"$STUB_MARK"
case "${STUB_MODE-ok}" in
ok)
	case "$1" in
	context) cat >/dev/null; echo '{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"stub memory"}}' ;;
	lookup) cat >/dev/null; echo '{"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":"stub lookup"}}' ;;
	serve) exit 0 ;;
	cost) sleep "${STUB_SLEEP-0}"; echo "stub cost $*" ;;
	*) cat >/dev/null ;;
	esac ;;
crash) echo '{"decision":"block","reason":"partial"}'; echo "boom" >&2; exit 3 ;;
hang) echo $$ >"$STUB_PIDFILE"; exec sleep 30 ;;
daemon-down) cat >/dev/null; echo "memory: cannot reach daemon" >&2; exit 1 ;;
serve-hangs) case "$1" in serve) echo $$ >"$STUB_PIDFILE.serve"; exec sleep 30 ;; context) cat >/dev/null; echo '{"ctx":1}' ;; esac ;;
esac
EOF
	chmod +x "$1"
}

# A fake release dir for tag v$ver with a good tarball (the binary plus the DuckDB
# extensions it loads, in ext/) and matching checksums. NO_EXT=1 leaves ext/ out.
exts="fts httpfs quack"
mkrelease() {
	rel=$1
	mkdir -p "$rel/v$ver" "$rel/work"
	mkstub "$rel/work/memory"
	if [ -n "${NO_EXT-}" ]; then
		tar -cJf "$rel/v$ver/$asset" -C "$rel/work" memory
	else
		mkdir -p "$rel/work/ext"
		for e in $exts; do echo "$e" >"$rel/work/ext/$e.duckdb_extension"; done
		tar -cJf "$rel/v$ver/$asset" -C "$rel/work" memory ext
	fi
	(cd "$rel/v$ver" && $sum "$asset" >checksums.txt)
}

# case <name>: fresh sandbox dirs and a clean environment.
newcase() {
	c=$tmp/$1
	mkdir -p "$c/data" "$c/path"
	: >"$c/mark"
	unset MEMORY_BIN STUB_MODE MEMORY_DIR MEMORY_EXT_DIR XDG_DATA_HOME NO_EXT
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

# settle <case>: wait (up to 15 s) for a background install to release its lock.
settle() {
	local i=0
	while [ -d "$tmp/$1/data/bin/.install-lock" ] && [ $i -lt 30 ]; do sleep 0.5; i=$((i + 1)); done
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
settle t1
hook t1 context
expect_open "binary missing (context)"
hook t1 mcp
expect_open "binary missing (mcp)"

# --- 2. positive control: the first session-start installs in the background and prints
# nothing; the installer starts the daemon; the next session-start passes context through
newcase t2
mkrelease "$tmp/t2/rel"
export MEMORY_RELEASE_URL=file://$tmp/t2/rel
hook t2 session-start
expect_open "first session-start returns at once while installing"
settle t2
if [ -x "$tmp/t2/data/bin/$ver/memory" ]; then ok "background install puts the binary in place"; else bad "background install puts the binary in place" "no binary; log: $(cat "$tmp/t2/data/memory.log" 2>/dev/null)"; fi
if grep -q '^serve ' "$tmp/t2/mark"; then ok "background install starts the daemon"; else bad "background install starts the daemon" "mark: $(cat "$tmp/t2/mark")"; fi
hook t2 session-start
if [ "$rc" -eq 0 ] && printf %s "$out" | grep -q 'stub memory'; then
	ok "next session-start passes context through"
else
	bad "next session-start passes context through" "rc=$rc out=$out"
fi
if jq -e '.hooks.SessionStart[0].hooks[0].timeout <= 15' "$root/hooks/hooks.json" >/dev/null 2>&1; then ok "SessionStart hook timeout is at most 15 s"; else bad "SessionStart hook timeout is at most 15 s" "$(jq -c '.hooks.SessionStart' "$root/hooks/hooks.json")"; fi
missing=
for e in $exts; do [ -f "$tmp/t2/data/bin/$ver/ext/$e.duckdb_extension" ] || missing="$missing $e"; done
if [ -z "$missing" ]; then ok "install unpacks the DuckDB extensions next to the binary"; else bad "install unpacks the DuckDB extensions next to the binary" "missing:$missing"; fi
# The launcher leaves MEMORY_DIR to the binary's own default and points it at the extensions
# installed with this version.
if grep -q "^context dir= ext=$tmp/t2/data/bin/$ver/ext\$" "$tmp/t2/mark"; then
	ok "binary runs with its own state dir default and the installed extension dir"
else
	bad "binary runs with its own state dir default and the installed extension dir" "$(cat "$tmp/t2/mark")"
fi

# --- 2b. a tarball without the extensions is a failed install: nothing installed, nothing run
newcase t2b
NO_EXT=1 mkrelease "$tmp/t2b/rel"
export MEMORY_RELEASE_URL=file://$tmp/t2b/rel
hook t2b session-start
expect_open "tarball without extensions (session-start)"
settle t2b
if [ -e "$tmp/t2b/data/bin/$ver/memory" ]; then bad "tarball without extensions installs no binary" "binary present"; else ok "tarball without extensions installs no binary"; fi
if [ -s "$tmp/t2b/mark" ]; then bad "tarball without extensions never executes" "stub ran"; else ok "tarball without extensions never executes"; fi

# --- 2c. PreToolUse lookup: passes context through, and is wired in hooks.json
newcase t2c
mkstub "$tmp/t2c/memory"
export MEMORY_BIN=$tmp/t2c/memory
hook t2c lookup
if [ "$rc" -eq 0 ] && printf %s "$out" | grep -q 'stub lookup'; then ok "lookup passes its context through"; else bad "lookup passes its context through" "rc=$rc out=$out"; fi
export STUB_MODE=crash
hook t2c lookup
expect_open "lookup crash prints nothing"
if jq -e '.hooks.PreToolUse[] | select(.matcher == "Edit|Write|NotebookEdit") | .hooks[] | select(.command | endswith("run.sh\" lookup"))' "$root/hooks/hooks.json" >/dev/null 2>&1; then
	ok "hooks.json runs lookup before Edit, Write and NotebookEdit"
else
	bad "hooks.json runs lookup before Edit, Write and NotebookEdit" "no matching PreToolUse entry"
fi

# --- 2d. PostCompact compact-capture: wired in hooks.json, prints nothing whatever the binary
# does, and a payload is never lost: with no binary (or a binary that fails) the raw payload
# is spooled for the daemon to ingest.
if jq -e '.hooks.PostCompact[0].hooks[] | select(.command | endswith("run.sh\" compact-capture"))' "$root/hooks/hooks.json" >/dev/null 2>&1; then
	ok "hooks.json runs compact-capture on PostCompact"
else
	bad "hooks.json runs compact-capture on PostCompact" "no PostCompact entry"
fi
payload='{"session_id":"abc-1","cwd":"/tmp","hook_event_name":"PostCompact","trigger":"manual","compact_summary":"we fixed it"}'
cc() { # <case> : run compact-capture with $payload
	c=$tmp/$1
	out=$(printf '%s' "$payload" | sh "$run_sh" compact-capture 2>"$c/stderr")
	rc=$?
}
newcase t2d
export MEMORY_DIR=$tmp/t2d/mem
cc t2d
expect_open "compact-capture with no binary prints nothing"
n=$(ls "$MEMORY_DIR/compactions"/*.json 2>/dev/null | wc -l)
if [ "$n" -eq 1 ] && grep -q 'we fixed it' "$MEMORY_DIR"/compactions/*.json; then ok "compact-capture with no binary spools the raw payload"; else bad "compact-capture with no binary spools the raw payload" "files=$n"; fi
if ls "$MEMORY_DIR/compactions"/.* >/dev/null 2>&1 && [ -n "$(find "$MEMORY_DIR/compactions" -name '.*.tmp' 2>/dev/null)" ]; then bad "no temp files left in the spool" "$(ls -a "$MEMORY_DIR/compactions")"; else ok "no temp files left in the spool"; fi
# The raw spool reaches its final name only by a rename from a dot-tmp file, so the daemon never
# reads a half-written copy: a shim mv records what it is asked to move.
newcase t2d-mv
real_mv=$(command -v mv)
mkdir -p "$tmp/t2d-mv/shim"
printf '#!/bin/sh\necho "$*" >>"%s"\nexec "%s" "$@"\n' "$tmp/t2d-mv/mvlog" "$real_mv" >"$tmp/t2d-mv/shim/mv"
chmod +x "$tmp/t2d-mv/shim/mv"
export MEMORY_DIR=$tmp/t2d-mv/mem
out=$(printf '%s' "$payload" | PATH="$tmp/t2d-mv/shim:$PATH" sh "$run_sh" compact-capture 2>/dev/null)
if grep -q '/\.raw-[^ ]*\.json\.tmp .*/raw-[^ ]*\.json$' "$tmp/t2d-mv/mvlog" 2>/dev/null; then ok "the raw spool is written to a dot-tmp name and renamed"; else bad "the raw spool is written to a dot-tmp name and renamed" "mv log: $(cat "$tmp/t2d-mv/mvlog" 2>&1)"; fi
newcase t2d-stub
mkstub "$tmp/t2d-stub/memory"
export MEMORY_BIN=$tmp/t2d-stub/memory MEMORY_DIR=$tmp/t2d-stub/mem
cc t2d-stub
expect_open "compact-capture with a working binary prints nothing"
if grep -q '^compact-capture ' "$tmp/t2d-stub/mark" && [ ! -d "$MEMORY_DIR/compactions" ]; then ok "compact-capture hands the payload to the binary, no shell spool"; else bad "compact-capture hands the payload to the binary, no shell spool" "mark: $(cat "$tmp/t2d-stub/mark")"; fi
export STUB_MODE=crash
cc t2d-stub
expect_open "compact-capture with a crashing binary prints nothing"
if [ "$(ls "$MEMORY_DIR/compactions"/*.json 2>/dev/null | wc -l)" -eq 1 ]; then ok "a failed compact-capture falls back to the raw spool"; else bad "a failed compact-capture falls back to the raw spool" "$(ls -a "$MEMORY_DIR/compactions" 2>&1)"; fi

# --- 3. checksum mismatch: never execute, never install
newcase t3
mkrelease "$tmp/t3/rel"
echo "0000000000000000000000000000000000000000000000000000000000000000  $asset" >"$tmp/t3/rel/v$ver/checksums.txt"
export MEMORY_RELEASE_URL=file://$tmp/t3/rel
hook t3 session-start
expect_open "checksum mismatch (session-start)"
settle t3
if [ -e "$tmp/t3/data/bin/$ver/memory" ]; then bad "checksum mismatch installs nothing" "binary present"; else ok "checksum mismatch installs nothing"; fi
if [ -s "$tmp/t3/mark" ]; then bad "checksum mismatch never executes the binary" "stub ran: $(cat "$tmp/t3/mark")"; else ok "checksum mismatch never executes the binary"; fi
hook t3 context
expect_open "checksum mismatch (context after failed install)"

# --- 3b. checksums.txt has no line for this asset
newcase t3b
mkrelease "$tmp/t3b/rel"
echo "deadbeef  some_other_asset.tar.gz" >"$tmp/t3b/rel/v$ver/checksums.txt"
export MEMORY_RELEASE_URL=file://$tmp/t3b/rel
hook t3b session-start
expect_open "checksums.txt lacks the asset"
settle t3b
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
settle t4
PATH=$old_path
expect_open "unsupported platform"
if [ -e "$tmp/t4/curl-called" ]; then bad "unsupported platform skips the download" "curl was called"; else ok "unsupported platform skips the download"; fi

# --- 4b. Linux without xz: GNU tar cannot unpack .tar.xz, so the install stops before the
# download and says why. PATH is a farm of links to every program except xz, and uname says
# Linux, so this runs on macOS CI too.
newcase t4b
mkdir -p "$tmp/t4b/path"
old_ifs=$IFS
IFS=:
for d in $PATH; do
	for f in "$d"/*; do
		n=${f##*/}
		[ "$n" = xz ] || [ -e "$tmp/t4b/path/$n" ] || [ ! -x "$f" ] || ln -s "$f" "$tmp/t4b/path/$n"
	done
done
IFS=$old_ifs
rm -f "$tmp/t4b/path/uname" "$tmp/t4b/path/curl"
cat >"$tmp/t4b/path/uname" <<'EOF'
#!/bin/sh
case "$1" in -m) echo x86_64 ;; *) echo Linux ;; esac
EOF
cat >"$tmp/t4b/path/curl" <<EOF
#!/bin/sh
echo curl >>"$tmp/t4b/curl-called"
exit 1
EOF
chmod +x "$tmp/t4b/path/uname" "$tmp/t4b/path/curl"
old_path=$PATH
PATH=$tmp/t4b/path
if command -v xz >/dev/null 2>&1; then xz_hidden=no; else xz_hidden=yes; fi
hook t4b session-start
settle t4b
PATH=$old_path
if [ "$xz_hidden" = yes ]; then ok "test PATH hides xz"; else bad "test PATH hides xz" "xz still found"; fi
expect_open "Linux without xz"
if [ -e "$tmp/t4b/curl-called" ]; then bad "Linux without xz skips the download" "curl was called"; else ok "Linux without xz skips the download"; fi
if grep -q 'xz not found (install xz-utils)' "$tmp/t4b/data/memory.log" 2>/dev/null; then
	ok "Linux without xz logs the fix"
else
	bad "Linux without xz logs the fix" "log: $(cat "$tmp/t4b/data/memory.log" 2>/dev/null)"
fi

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
[ -s "$tmp/t8/pid.serve" ] && kill "$(cat "$tmp/t8/pid.serve")" 2>/dev/null

# --- 9. data directory not writable
newcase t9
mkdir -p "$tmp/t9/ro"
chmod 500 "$tmp/t9/ro"
export CLAUDE_PLUGIN_DATA=$tmp/t9/ro/nested
mkrelease "$tmp/t9/rel"
export MEMORY_RELEASE_URL=file://$tmp/t9/rel
hook t9 session-start
settle t9
expect_open "data dir not writable"
chmod 700 "$tmp/t9/ro"

# --- 10. diagnostics go to a log or stderr, never stdout (covered above); a failure leaves a trace
newcase t10
hook t10 session-start
settle t10
if [ -s "$tmp/t10/stderr" ] || [ -s "$tmp/t10/data/memory.log" ]; then ok "failure leaves a diagnostic in stderr or the log"; else bad "failure leaves a diagnostic in stderr or the log" "nothing written"; fi

# A curl stub for the next cases: records each call, then sleeps CURL_SLEEP seconds and fails.
mkcurl() {
	mkdir -p "$1"
	cat >"$1/curl" <<EOF
#!/bin/sh
echo curl >>"$1/curl-called"
sleep \${CURL_SLEEP:-0}
exit 1
EOF
	chmod +x "$1/curl"
}

# --- 11. a slow download does not hold up session-start
newcase t11
mkcurl "$tmp/t11/path"
old_path=$PATH
PATH=$tmp/t11/path:$PATH CURL_SLEEP=4 hook t11 session-start
PATH=$old_path
expect_open "slow download (session-start)"
if [ "$secs" -le 1 ]; then ok "session-start returns before a slow download finishes (${secs}s)"; else bad "session-start returns before a slow download finishes" "took ${secs}s"; fi
settle t11

# --- 12. an install in progress is not started twice; a stale lock is taken over
newcase t12
mkcurl "$tmp/t12/path"
mkdir -p "$tmp/t12/data/bin/.install-lock"
old_path=$PATH
PATH=$tmp/t12/path:$PATH hook t12 session-start
sleep 1
if [ -e "$tmp/t12/path/curl-called" ]; then bad "a held install lock starts no second download" "curl was called"; else ok "a held install lock starts no second download"; fi
touch -t 202001010000 "$tmp/t12/data/bin/.install-lock"
PATH=$tmp/t12/path:$PATH hook t12 session-start
PATH=$old_path
settle t12
if [ -e "$tmp/t12/path/curl-called" ]; then ok "a stale install lock is taken over"; else bad "a stale install lock is taken over" "curl was never called"; fi
if [ -d "$tmp/t12/data/bin/.install-lock" ]; then bad "the installer releases its lock" "lock still held"; else ok "the installer releases its lock"; fi

# --- 13. `cost` is run by a person: it never waits for stdin (a terminal never sends EOF),
# passes its flags through, and is not killed by the hook watchdog
newcase t13
mkrelease "$tmp/t13/rel"
mkdir -p "$tmp/t13/data/bin/$ver"
mkstub "$tmp/t13/data/bin/$ver/memory"
c=$tmp/t13
# No `timeout` on stock macOS: a background run with a stdin that never closes, killed after 6 s.
(sleep 8) | STUB_SLEEP=3 sh "$run_sh" cost --days 3 >"$c/out" 2>"$c/stderr" &
cpid=$!
i=0
while kill -0 "$cpid" 2>/dev/null && [ $i -lt 12 ]; do sleep 0.5; i=$((i + 1)); done
if kill -0 "$cpid" 2>/dev/null; then rc=124; kill "$cpid" 2>/dev/null; else wait "$cpid"; rc=$?; fi
out=$(cat "$c/out")
if [ "$rc" -eq 0 ] && [ "$out" = "stub cost cost --days 3" ]; then
	ok "cost ignores an open stdin, keeps its flags and outlives the 2 s hook limit"
else
	bad "cost ignores an open stdin, keeps its flags and outlives the 2 s hook limit" "rc=$rc out=$out"
fi

# --- 14. `cost` from a terminal (no CLAUDE_PLUGIN_ROOT or CLAUDE_PLUGIN_DATA) finds the data
# dir Claude Code uses: <config>/plugins/data/memory-<marketplace>, where the plugin is cached
# at <config>/plugins/cache/<marketplace>/memory/<version>.
# plugin_copy <dir>: the plugin's launcher and manifest, installed at <dir>.
plugin_copy() {
	mkdir -p "$1"
	cp -R "$root/scripts" "$root/.claude-plugin" "$1/"
}
# terminal_cost <case> <run.sh>: run `cost --days 7` as a person does; sets rc and out.
terminal_cost() {
	(unset CLAUDE_PLUGIN_ROOT CLAUDE_PLUGIN_DATA && sh "$2" cost --days 7 </dev/null >"$tmp/$1/out" 2>"$tmp/$1/stderr")
	rc=$?
	out=$(cat "$tmp/$1/out")
}
want_cost="stub cost cost --days 7"
for cfgcase in home config-dir; do
	newcase "t14-$cfgcase"
	c=$tmp/t14-$cfgcase
	if [ "$cfgcase" = home ]; then
		unset CLAUDE_CONFIG_DIR
		cfg=$HOME/.claude
	else
		export CLAUDE_CONFIG_DIR=$c/cfg
		cfg=$CLAUDE_CONFIG_DIR
	fi
	plugin_copy "$cfg/plugins/cache/broodhall/memory/$ver"
	mkdir -p "$cfg/plugins/data/memory-broodhall/bin/$ver" "$cfg/plugins/data/memory"
	mkstub "$cfg/plugins/data/memory-broodhall/bin/$ver/memory"
	terminal_cost "t14-$cfgcase" "$cfg/plugins/cache/broodhall/memory/$ver/scripts/run.sh"
	if [ "$rc" -eq 0 ] && [ "$out" = "$want_cost" ]; then
		ok "terminal cost finds plugins/data/memory-<marketplace> ($cfgcase)"
	else
		bad "terminal cost finds plugins/data/memory-<marketplace> ($cfgcase)" "rc=$rc out=$out stderr=$(cat "$c/stderr")"
	fi
	unset CLAUDE_CONFIG_DIR
done

# A launcher outside the plugin cache (a checkout) uses the memory-* data dir that holds this
# version's binary, else the plain memory dir, under CLAUDE_CONFIG_DIR.
newcase t14-checkout
c=$tmp/t14-checkout
export CLAUDE_CONFIG_DIR=$c/cfg
cfg=$CLAUDE_CONFIG_DIR
plugin_copy "$c/checkout/plugins/memory"
mkdir -p "$cfg/plugins/data/memory-other" "$cfg/plugins/data/memory-inline/bin/$ver"
mkstub "$cfg/plugins/data/memory-inline/bin/$ver/memory"
terminal_cost t14-checkout "$c/checkout/plugins/memory/scripts/run.sh"
if [ "$rc" -eq 0 ] && [ "$out" = "$want_cost" ]; then ok "terminal cost from a checkout finds the memory-* dir with a binary"; else bad "terminal cost from a checkout finds the memory-* dir with a binary" "rc=$rc out=$out"; fi
rm -rf "$cfg/plugins/data/memory-inline"
mkdir -p "$cfg/plugins/data/memory/bin/$ver"
mkstub "$cfg/plugins/data/memory/bin/$ver/memory"
terminal_cost t14-checkout "$c/checkout/plugins/memory/scripts/run.sh"
if [ "$rc" -eq 0 ] && [ "$out" = "$want_cost" ]; then ok "terminal cost from a checkout falls back to plugins/data/memory"; else bad "terminal cost from a checkout falls back to plugins/data/memory" "rc=$rc out=$out"; fi
unset CLAUDE_CONFIG_DIR

# With nothing installed yet, a cached launcher still picks memory-<marketplace> (its log
# lands there), not the plain memory dir.
newcase t14-fresh
c=$tmp/t14-fresh
cfg=$HOME/.claude
plugin_copy "$cfg/plugins/cache/broodhall/memory/$ver"
terminal_cost t14-fresh "$cfg/plugins/cache/broodhall/memory/$ver/scripts/run.sh"
if [ -f "$cfg/plugins/data/memory-broodhall/memory.log" ] && [ ! -e "$cfg/plugins/data/memory" ]; then
	ok "a cached launcher with no binary uses plugins/data/memory-<marketplace>"
else
	bad "a cached launcher with no binary uses plugins/data/memory-<marketplace>" "$(ls "$cfg/plugins/data" 2>&1)"
fi

# Inside Claude Code CLAUDE_PLUGIN_DATA still wins over the derived dir.
newcase t14-hook
c=$tmp/t14-hook
cfg=$HOME/.claude
plugin_copy "$cfg/plugins/cache/broodhall/memory/$ver"
mkdir -p "$cfg/plugins/data/memory-broodhall/bin/$ver" "$c/data/bin/$ver"
mkstub "$cfg/plugins/data/memory-broodhall/bin/$ver/memory"
printf '#!/bin/sh\necho from-plugin-data\n' >"$c/data/bin/$ver/memory"
chmod +x "$c/data/bin/$ver/memory"
out=$(CLAUDE_PLUGIN_ROOT=$cfg/plugins/cache/broodhall/memory/$ver sh "$cfg/plugins/cache/broodhall/memory/$ver/scripts/run.sh" cost </dev/null 2>/dev/null)
if [ "$out" = from-plugin-data ]; then ok "CLAUDE_PLUGIN_DATA wins over the derived data dir"; else bad "CLAUDE_PLUGIN_DATA wins over the derived data dir" "out=$out"; fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
