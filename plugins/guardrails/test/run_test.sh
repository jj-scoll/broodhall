#!/bin/bash
# Launcher tests for plugins/guardrails/scripts/run.sh. No dependencies beyond sh, tar, curl,
# sha256sum or shasum. Run: bash plugins/guardrails/test/run_test.sh
#
# Contract under test: guards fail CLOSED. With no usable binary (never installed, failed
# download, checksum mismatch) every guard (pre-tool-use, read-dedup, large-read-guard,
# config-gate) exits 2 with a stderr message that says what failed, how to retry and how to
# opt out; the opt-out (fail_open) restores a silent exit 0. The other hooks exit 0 silently,
# and `install` itself never fails the session. An unverified binary is never executed.
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

# A stub binary: records its argv, the env the launcher sets and stdin, exits 3. `doctor` is
# silent, as a healthy install's doctor is (install_test.sh covers what doctor prints).
mkstub() {
	cat >"$1" <<'EOF'
#!/bin/sh
[ "${1-}" = doctor ] && exit 0
echo "args=$* off=${CLAUDE_TOOLS_OFF-unset} allow=${CLAUDE_TOOLS_PII_ALLOW-unset} observe=${CLAUDE_TOOLS_OBSERVE-unset} stdin=$(cat)"
exit 3
EOF
	chmod +x "$1"
}

# mkrelease <dir> [checksum]: a fake release for v$ver, laid out like the GitHub download URL.
mkrelease() {
	rel=$1
	mkdir -p "$rel/v$ver" "$rel/work"
	mkstub "$rel/work/claude-tools"
	tar -czf "$rel/v$ver/$asset" -C "$rel/work" claude-tools
	if [ -n "${2-}" ]; then
		echo "$2  $asset" >"$rel/v$ver/checksums.txt"
	else
		(cd "$rel/v$ver" && $sum "$asset" >checksums.txt)
	fi
}

# newcase <name>: fresh sandbox dirs and a clean environment.
newcase() {
	c=$tmp/$1
	mkdir -p "$c/data" "$c/home"
	unset CLAUDE_TOOLS_BIN CLAUDE_TOOLS_OFF CLAUDE_TOOLS_FAIL_OPEN CLAUDE_TOOLS_PII_ALLOW
	unset CLAUDE_TOOLS_OBSERVE CLAUDE_PLUGIN_OPTION_OBSERVE CLAUDE_PLUGIN_OPTION_FAIL_OPEN CLAUDE_PLUGIN_OPTION_DISABLED_GUARDS CLAUDE_PLUGIN_OPTION_ALLOWED_EMAIL_DOMAINS
	export CLAUDE_PLUGIN_ROOT=$root CLAUDE_PLUGIN_DATA=$c/data HOME=$c/home
	export CLAUDE_TOOLS_RELEASE_URL=file://$c/no-such-release
}

# hook <args...>: run the launcher as Claude Code does (JSON on stdin); sets rc, out, err.
hook() {
	sh "$run_sh" "$@" <<<'{"tool_name":"Bash"}' >"$c/out" 2>"$c/err"
	rc=$?
	out=$(cat "$c/out")
	err=$(cat "$c/err")
}

expect_blocked() { # <name>: rc 2, stdout empty, stderr says what/retry/opt-out
	if [ "$rc" != 2 ]; then bad "$1" "exit $rc, want 2"; return; fi
	if [ -n "$out" ]; then bad "$1" "stdout not empty: $out"; return; fi
	for want in "$ver" install fail_open; do
		case $err in *"$want"*) ;; *) bad "$1" "stderr lacks '$want': $err"; return ;; esac
	done
	ok "$1"
}

expect_silent_allow() { # <name>
	if [ "$rc" = 0 ] && [ -z "$out" ] && [ -z "$err" ]; then ok "$1"; else bad "$1" "exit $rc stdout '$out' stderr '$err'"; fi
}

# --- default off list is empty -----------------------------------------------------------
newcase defaults
mkstub "$c/stub"
CLAUDE_TOOLS_BIN=$c/stub hook pre-tool-use
case $out in *"off= "*) ok "default CLAUDE_TOOLS_OFF is empty" ;; *) bad "default CLAUDE_TOOLS_OFF is empty" "$out" ;; esac
CLAUDE_TOOLS_OFF=pii CLAUDE_TOOLS_BIN=$c/stub hook pre-tool-use
case $out in *"off=pii "*) ok "CLAUDE_TOOLS_OFF env still overrides" ;; *) bad "CLAUDE_TOOLS_OFF env still overrides" "$out" ;; esac
CLAUDE_PLUGIN_OPTION_DISABLED_GUARDS=egress CLAUDE_TOOLS_OFF=pii CLAUDE_TOOLS_BIN=$c/stub hook pre-tool-use
case $out in *"off=egress "*) ok "disabled_guards option beats env" ;; *) bad "disabled_guards option beats env" "$out" ;; esac
CLAUDE_TOOLS_BIN=$c/stub hook pre-tool-use
case $out in *"observe= "*) ok "observe defaults to empty (off)" ;; *) bad "observe defaults to empty (off)" "$out" ;; esac
CLAUDE_PLUGIN_OPTION_OBSERVE=true CLAUDE_TOOLS_OBSERVE=false CLAUDE_TOOLS_BIN=$c/stub hook pre-tool-use
case $out in *"observe=true "*) ok "observe option beats env" ;; *) bad "observe option beats env" "$out" ;; esac
grep -q '"default": "pii' "$root/.claude-plugin/plugin.json" && bad "plugin.json default" "disabled_guards still defaults to a family list"

# --- no binary: fail closed --------------------------------------------------------------
for sub in pre-tool-use read-dedup large-read-guard config-gate; do
	newcase "missing-$sub"
	hook "$sub"
	expect_blocked "no binary: $sub exits 2 with guidance"
done
# Not guards: exit 2 at Stop would keep Claude from ending its turn, at PreCompact would block
# compaction.
for sub in retry-breaker ask-outcome record-edit gate receipt compact-guard session-env; do
	newcase "missing-$sub"
	hook "$sub"
	expect_silent_allow "no binary: $sub exits 0 silently"
done

newcase missing-optout-option
CLAUDE_PLUGIN_OPTION_FAIL_OPEN=true hook pre-tool-use
expect_silent_allow "no binary + fail_open=true allows silently"

newcase missing-optout-env
CLAUDE_TOOLS_FAIL_OPEN=1 hook pre-tool-use
expect_silent_allow "no binary + CLAUDE_TOOLS_FAIL_OPEN=1 allows silently"

newcase missing-optout-false
CLAUDE_PLUGIN_OPTION_FAIL_OPEN=false hook pre-tool-use
expect_blocked "no binary + fail_open=false still blocks"

# --- install failures stay non-fatal, and the guards then fail closed ---------------------
newcase install-download-fails
hook install
case $out in *'"systemMessage"'*) ok "install: failed download warns with systemMessage" ;; *) bad "install: failed download warns with systemMessage" "rc=$rc out=$out" ;; esac
[ "$rc" = 0 ] || bad "install: failed download exits 0" "exit $rc"
hook pre-tool-use
expect_blocked "after failed download: pre-tool-use blocks"

newcase install-bad-checksum
mkrelease "$c/rel" "$(printf '0%.0s' $(seq 64))"
CLAUDE_TOOLS_RELEASE_URL=file://$c/rel hook install
if [ "$rc" = 0 ] && case $out in *"checksum mismatch"*) true ;; *) false ;; esac; then ok "install: bad checksum warns, exits 0"; else bad "install: bad checksum warns, exits 0" "rc=$rc out=$out"; fi
if [ -e "$c/data/bin/$ver/claude-tools" ]; then bad "bad checksum installs nothing" "binary present"; else ok "bad checksum installs nothing"; fi
hook pre-tool-use
expect_blocked "after bad checksum: pre-tool-use blocks"
CLAUDE_PLUGIN_OPTION_FAIL_OPEN=true hook pre-tool-use
expect_silent_allow "after bad checksum + fail_open: allows"

newcase install-fail-open-message
CLAUDE_PLUGIN_OPTION_FAIL_OPEN=true hook install
case $out in *"guards are off"*) ok "install failure message with fail_open says guards are off" ;; *) bad "install failure message with fail_open says guards are off" "$out" ;; esac
hook install
case $out in *"blocked"*) ok "install failure message without fail_open says calls are blocked" ;; *) bad "install failure message without fail_open says calls are blocked" "$out" ;; esac

# --- good install ------------------------------------------------------------------------
newcase install-good
mkrelease "$c/rel"
export CLAUDE_TOOLS_RELEASE_URL=file://$c/rel
hook install
if [ "$rc" = 0 ] && [ -z "$out" ] && [ -x "$c/data/bin/$ver/claude-tools" ]; then ok "install: good release installs silently"; else bad "install: good release installs silently" "rc=$rc out=$out"; fi
hook pre-tool-use
if [ "$rc" = 3 ] && case $out in *"args=pre-tool-use"*"stdin={\"tool_name\":\"Bash\"}"*) true ;; *) false ;; esac; then ok "installed binary runs with args, stdin and its exit code"; else bad "installed binary runs" "rc=$rc out=$out"; fi
CLAUDE_TOOLS_RELEASE_URL=file:///nonexistent hook install
expect_silent_allow "install: already installed is silent"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" = 0 ]
