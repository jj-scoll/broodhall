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
asset=guardrails_${os}_$arch.tar.gz

# A stub binary: records its argv, the env the launcher sets and stdin, exits 3. `doctor` is
# silent, as a healthy install's doctor is (install_test.sh covers what doctor prints).
mkstub() {
	cat >"$1" <<'EOF'
#!/bin/sh
[ "${1-}" = doctor ] && exit 0
echo "args=$* off=${CLAUDE_TOOLS_OFF-unset} allow=${CLAUDE_TOOLS_PII_ALLOW-unset} observe=${CLAUDE_TOOLS_OBSERVE-unset} goff=${GUARDRAILS_OFF-unset} gallow=${GUARDRAILS_PII_ALLOW-unset} stdin=$(cat)"
exit 3
EOF
	chmod +x "$1"
}

# mkrelease <dir> [checksum] [name]: a fake release for v$ver, laid out like the GitHub
# download URL. name is the binary and asset prefix: guardrails, or claude-tools for a release
# made before the rename.
mkrelease() {
	rel=$1 name=${3:-guardrails}
	a=${name}_${os}_$arch.tar.gz
	mkdir -p "$rel/v$ver" "$rel/work"
	mkstub "$rel/work/$name"
	tar -czf "$rel/v$ver/$a" -C "$rel/work" "$name"
	if [ -n "${2-}" ]; then
		echo "$2  $a" >"$rel/v$ver/checksums.txt"
	else
		(cd "$rel/v$ver" && $sum "$a" >checksums.txt)
	fi
}

# newcase <name>: fresh sandbox dirs and a clean environment.
newcase() {
	c=$tmp/$1
	mkdir -p "$c/data" "$c/home"
	unset CLAUDE_TOOLS_BIN CLAUDE_TOOLS_OFF CLAUDE_TOOLS_FAIL_OPEN CLAUDE_TOOLS_PII_ALLOW
	unset GUARDRAILS_BIN GUARDRAILS_OFF GUARDRAILS_FAIL_OPEN GUARDRAILS_PII_ALLOW GUARDRAILS_OBSERVE GUARDRAILS_RELEASE_URL
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
# The GUARDRAILS_* names beat the CLAUDE_TOOLS_* ones, and both are exported with the same value:
# a binary from before the rename reads only CLAUDE_TOOLS_*.
GUARDRAILS_OFF=pii GUARDRAILS_BIN=$c/stub hook pre-tool-use
case $out in *"off=pii "*"goff=pii "*) ok "GUARDRAILS_OFF and GUARDRAILS_BIN are honored, both names exported" ;; *) bad "GUARDRAILS_OFF and GUARDRAILS_BIN are honored, both names exported" "$out" ;; esac
GUARDRAILS_OFF=egress CLAUDE_TOOLS_OFF=pii GUARDRAILS_BIN=$c/stub hook pre-tool-use
case $out in *"off=egress "*"goff=egress "*) ok "GUARDRAILS_OFF beats CLAUDE_TOOLS_OFF" ;; *) bad "GUARDRAILS_OFF beats CLAUDE_TOOLS_OFF" "$out" ;; esac
GUARDRAILS_OFF= CLAUDE_TOOLS_OFF=pii GUARDRAILS_BIN=$c/stub hook pre-tool-use
case $out in *"off= "*"goff= "*) ok "GUARDRAILS_OFF set empty beats CLAUDE_TOOLS_OFF" ;; *) bad "GUARDRAILS_OFF set empty beats CLAUDE_TOOLS_OFF" "$out" ;; esac
CLAUDE_PLUGIN_OPTION_ALLOWED_EMAIL_DOMAINS=me@x.com GUARDRAILS_PII_ALLOW=no@y.com GUARDRAILS_BIN=$c/stub hook pre-tool-use
case $out in *"allow=me@x.com "*"gallow=me@x.com "*) ok "allowed_email_domains option beats GUARDRAILS_PII_ALLOW" ;; *) bad "allowed_email_domains option beats GUARDRAILS_PII_ALLOW" "$out" ;; esac
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

# The messages name the binary guardrails, not its old name claude-tools.
newcase messages-name
hook pre-tool-use
case $err in *"the guard binary guardrails $ver is not installed ($c/data/bin/$ver/guardrails)"*) ok "BLOCKED message names guardrails and its path" ;; *) bad "BLOCKED message names guardrails and its path" "$err" ;; esac
case $err in *claude-tools*) bad "BLOCKED message has no claude-tools" "$err" ;; *) ok "BLOCKED message has no claude-tools" ;; esac
hook install
case $out in *"could not install guardrails $ver (download of guardrails_${os}_$arch.tar.gz failed)"*) ok "install failure names guardrails and its asset" ;; *) bad "install failure names guardrails and its asset" "$out" ;; esac
case $out in *claude-tools*) bad "install failure has no claude-tools" "$out" ;; *) ok "install failure has no claude-tools" ;; esac

# An older version's binary, under the old name, is not this version's binary: a guard still
# runs it while this version installs (see the version-bump section below).
newcase old-version-old-name
mkdir -p "$c/data/bin/0.0.1"
mkstub "$c/data/bin/0.0.1/claude-tools"
hook pre-tool-use
if [ "$rc" = 3 ] && case $out in *"args=pre-tool-use"*) true ;; *) false ;; esac; then ok "only an older version's claude-tools: pre-tool-use runs it"; else bad "only an older version's claude-tools: pre-tool-use runs it" "rc=$rc out=$out err=$err"; fi

newcase missing-optout-option
CLAUDE_PLUGIN_OPTION_FAIL_OPEN=true hook pre-tool-use
expect_silent_allow "no binary + fail_open=true allows silently"

newcase missing-optout-env
CLAUDE_TOOLS_FAIL_OPEN=1 hook pre-tool-use
expect_silent_allow "no binary + CLAUDE_TOOLS_FAIL_OPEN=1 allows silently"

newcase missing-optout-new-env
GUARDRAILS_FAIL_OPEN=1 hook pre-tool-use
expect_silent_allow "no binary + GUARDRAILS_FAIL_OPEN=1 allows silently"
case $(sed -n '/Opt out/p' "$run_sh") in *GUARDRAILS_FAIL_OPEN*) ok "opt-out message names GUARDRAILS_FAIL_OPEN" ;; *) bad "opt-out message names GUARDRAILS_FAIL_OPEN" "" ;; esac

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
if [ -e "$c/data/bin/$ver/guardrails" ]; then bad "bad checksum installs nothing" "binary present"; else ok "bad checksum installs nothing"; fi
hook pre-tool-use
expect_blocked "after bad checksum: pre-tool-use blocks"
CLAUDE_PLUGIN_OPTION_FAIL_OPEN=true hook pre-tool-use
expect_silent_allow "after bad checksum + fail_open: allows"
CLAUDE_TOOLS_FAIL_OPEN=yes hook pre-tool-use
expect_silent_allow "after bad checksum + CLAUDE_TOOLS_FAIL_OPEN=yes: allows"
CLAUDE_TOOLS_FAIL_OPEN=yes hook install
case $out in *"guards are off"*) ok "install failure with CLAUDE_TOOLS_FAIL_OPEN says guards are off" ;; *) bad "install failure with CLAUDE_TOOLS_FAIL_OPEN says guards are off" "$out" ;; esac

newcase install-new-release-url
mkrelease "$c/rel"
GUARDRAILS_RELEASE_URL=file://$c/rel hook install
if [ -x "$c/data/bin/$ver/guardrails" ]; then ok "GUARDRAILS_RELEASE_URL is used for the download"; else bad "GUARDRAILS_RELEASE_URL is used for the download" "rc=$rc out=$out"; fi

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
if [ "$rc" = 0 ] && [ -z "$out" ] && [ -x "$c/data/bin/$ver/guardrails" ]; then ok "install: good release installs silently"; else bad "install: good release installs silently" "rc=$rc out=$out"; fi
hook pre-tool-use
if [ "$rc" = 3 ] && case $out in *"args=pre-tool-use"*"stdin={\"tool_name\":\"Bash\"}"*) true ;; *) false ;; esac; then ok "installed binary runs with args, stdin and its exit code"; else bad "installed binary runs" "rc=$rc out=$out"; fi
CLAUDE_TOOLS_RELEASE_URL=file:///nonexistent hook install
expect_silent_allow "install: already installed is silent"

# --- a release made before the rename ----------------------------------------------------
# Main can advertise a version whose release predates the rename (its only asset is
# claude-tools_<os>_<arch>.tar.gz holding claude-tools) until the next release ships.
newcase install-old-release
mkrelease "$c/rel" "" claude-tools
CLAUDE_TOOLS_RELEASE_URL=file://$c/rel hook install
if [ "$rc" = 0 ] && [ -z "$out" ] && [ -x "$c/data/bin/$ver/guardrails" ]; then ok "install: a pre-rename release installs as bin/<ver>/guardrails"; else bad "install: a pre-rename release installs as bin/<ver>/guardrails" "rc=$rc out=$out"; fi
grep -q '  guardrails$' "$c/data/bin/$ver/guardrails.sha256" 2>/dev/null && ok "pre-rename release: the record names guardrails" || bad "pre-rename release: the record names guardrails" "$(cat "$c/data/bin/$ver/guardrails.sha256" 2>&1)"
hook pre-tool-use
if [ "$rc" = 3 ] && case $out in *"args=pre-tool-use"*) true ;; *) false ;; esac; then ok "pre-rename release: the installed binary runs"; else bad "pre-rename release: the installed binary runs" "rc=$rc out=$out"; fi

newcase install-old-release-bad-checksum
mkrelease "$c/rel" "$(printf '0%.0s' $(seq 64))" claude-tools
CLAUDE_TOOLS_RELEASE_URL=file://$c/rel hook install
if [ "$rc" = 0 ] && [ ! -e "$c/data/bin/$ver/guardrails" ] && case $out in *"checksum mismatch"*) true ;; *) false ;; esac; then ok "pre-rename release with a bad checksum installs nothing"; else bad "pre-rename release with a bad checksum installs nothing" "rc=$rc out=$out"; fi

# This version's binary, installed under the old name by the launcher before the rename.
newcase same-version-old-name
mkdir -p "$c/data/bin/$ver"
mkstub "$c/data/bin/$ver/claude-tools"
hook pre-tool-use
if [ "$rc" = 3 ] && case $out in *"args=pre-tool-use"*) true ;; *) false ;; esac; then ok "this version's claude-tools runs"; else bad "this version's claude-tools runs" "rc=$rc out=$out err=$err"; fi
hook install
expect_silent_allow "install: this version's claude-tools counts as installed"

# --- run from a terminal (no CLAUDE_PLUGIN_ROOT or CLAUDE_PLUGIN_DATA) -----------------------
# The launcher finds the data dir Claude Code uses, <config>/plugins/data/guardrails-<marketplace>,
# from its own place in <config>/plugins/cache/<marketplace>/guardrails/<version>.
for cfgcase in home config-dir; do
	newcase "terminal-$cfgcase"
	if [ "$cfgcase" = home ]; then cfg=$HOME/.claude; else cfg=$c/cfg; fi
	p=$cfg/plugins/cache/broodhall/guardrails/$ver
	mkdir -p "$p" "$cfg/plugins/data/guardrails-broodhall/bin/$ver" "$cfg/plugins/data/guardrails"
	cp -R "$root/scripts" "$root/.claude-plugin" "$p/"
	mkstub "$cfg/plugins/data/guardrails-broodhall/bin/$ver/guardrails"
	if [ "$cfgcase" = home ]; then
		out=$(unset CLAUDE_PLUGIN_ROOT CLAUDE_PLUGIN_DATA CLAUDE_CONFIG_DIR && sh "$p/scripts/run.sh" version </dev/null 2>&1)
	else
		out=$(unset CLAUDE_PLUGIN_ROOT CLAUDE_PLUGIN_DATA && CLAUDE_CONFIG_DIR=$cfg sh "$p/scripts/run.sh" version </dev/null 2>&1)
	fi
	case $out in *"args=version"*) ok "terminal run finds plugins/data/guardrails-<marketplace> ($cfgcase)" ;; *) bad "terminal run finds plugins/data/guardrails-<marketplace> ($cfgcase)" "out=$out" ;; esac
	# With nothing installed, the BLOCKED message (and so `run.sh install`) names that dir too.
	rm -rf "$cfg/plugins/data"
	out=$(unset CLAUDE_PLUGIN_ROOT CLAUDE_PLUGIN_DATA && CLAUDE_CONFIG_DIR=$cfg sh "$p/scripts/run.sh" pre-tool-use </dev/null 2>&1)
	case $out in *"/plugins/data/guardrails-broodhall/bin/$ver/guardrails"*) ok "no binary: terminal run targets guardrails-<marketplace> ($cfgcase)" ;; *) bad "no binary: terminal run targets guardrails-<marketplace> ($cfgcase)" "out=$out" ;; esac
done

# A checkout (outside the plugin cache) uses the guardrails-* data dir under CLAUDE_CONFIG_DIR
# that holds this version's binary.
newcase terminal-checkout
cfg=$c/cfg
mkdir -p "$c/checkout/plugins/guardrails" "$cfg/plugins/data/guardrails-inline/bin/$ver" "$cfg/plugins/data/guardrails"
cp -R "$root/scripts" "$root/.claude-plugin" "$c/checkout/plugins/guardrails/"
mkstub "$cfg/plugins/data/guardrails-inline/bin/$ver/guardrails"
out=$(unset CLAUDE_PLUGIN_ROOT CLAUDE_PLUGIN_DATA && CLAUDE_CONFIG_DIR=$cfg sh "$c/checkout/plugins/guardrails/scripts/run.sh" version </dev/null 2>&1)
case $out in *"args=version"*) ok "terminal run from a checkout finds the guardrails-* dir with a binary" ;; *) bad "terminal run from a checkout finds the guardrails-* dir with a binary" "out=$out" ;; esac
rm "$cfg/plugins/data/guardrails-inline/bin/$ver/guardrails"
mkstub "$cfg/plugins/data/guardrails-inline/bin/$ver/claude-tools"
out=$(unset CLAUDE_PLUGIN_ROOT CLAUDE_PLUGIN_DATA && CLAUDE_CONFIG_DIR=$cfg sh "$c/checkout/plugins/guardrails/scripts/run.sh" version </dev/null 2>&1)
case $out in *"args=version"*) ok "terminal run from a checkout finds this version's claude-tools" ;; *) bad "terminal run from a checkout finds this version's claude-tools" "out=$out" ;; esac
# Another version's claude-tools does not make a guardrails-* dir this version's.
newcase terminal-checkout-other-version
cfg=$c/cfg
mkdir -p "$c/checkout/plugins/guardrails" "$cfg/plugins/data/guardrails-old/bin/0.0.1" "$cfg/plugins/data/guardrails/bin/$ver"
cp -R "$root/scripts" "$root/.claude-plugin" "$c/checkout/plugins/guardrails/"
mkstub "$cfg/plugins/data/guardrails-old/bin/0.0.1/claude-tools"
mkstub "$cfg/plugins/data/guardrails/bin/$ver/guardrails"
out=$(unset CLAUDE_PLUGIN_ROOT CLAUDE_PLUGIN_DATA && CLAUDE_CONFIG_DIR=$cfg sh "$c/checkout/plugins/guardrails/scripts/run.sh" version </dev/null 2>&1)
case $out in *"args=version"*) ok "terminal run from a checkout skips a dir with only another version's claude-tools" ;; *) bad "terminal run from a checkout skips a dir with only another version's claude-tools" "out=$out" ;; esac

# --- a version bump: this version's binary is not downloaded yet ---------------------------
# Failure A1: SessionStart runs `install` and `config-gate` in parallel, so the fail-closed
# config-gate ran before the download finished and blocked. Failure A2: /reload-plugins moves
# the session to the new version without firing SessionStart, so nothing downloads it and every
# PreToolUse blocked. Contract: a guard runs the newest binary already installed for another
# version while this version installs, starts that install itself, and with no binary at all
# waits (bounded) for the install instead of blocking at once.

# mkid <path> <label>: a stub binary that says which one it is.
mkid() {
	mkdir -p "${1%/*}"
	cat >"$1" <<EOF
#!/bin/sh
[ "\${1-}" = doctor ] && exit 0
echo "from=$2 args=\$* observe=\${CLAUDE_TOOLS_OBSERVE-} stdin=\$(cat)"
exit 3
EOF
	chmod +x "$1"
}

# mkslowcurl <dir> <seconds>: a curl on PATH that logs its arguments, sleeps, then runs the real one.
mkslowcurl() {
	realcurl=$(command -v curl)
	mkdir -p "$1"
	cat >"$1/curl" <<EOF
#!/bin/sh
echo "\$*" >>"$1/curl.log"
sleep $2
exec $realcurl "\$@"
EOF
	chmod +x "$1/curl"
}

waitfor() { # <path> : wait up to 10 s for an executable
	w=0
	while [ ! -x "$1" ] && [ "$w" -lt 100 ]; do sleep 0.1; w=$((w + 1)); done
}

ran_stub() { [ "$rc" = 3 ] && case $out in *"args=$1"*) true ;; *) false ;; esac; }

# A2: no SessionStart. The old version's binary runs the guard and the guard starts the install.
newcase bump-reload
mkrelease "$c/rel"
mkid "$c/data/bin/0.0.1/guardrails" old1
export CLAUDE_TOOLS_RELEASE_URL=file://$c/rel
hook pre-tool-use
if ran_stub pre-tool-use && case $out in *"from=old1"*"stdin={\"tool_name\":\"Bash\"}"*) true ;; *) false ;; esac; then ok "version bump: pre-tool-use runs the older installed binary, not a block"; else bad "version bump: pre-tool-use runs the older installed binary, not a block" "rc=$rc out=$out err=$err"; fi
waitfor "$c/data/bin/$ver/guardrails"
if [ -x "$c/data/bin/$ver/guardrails" ]; then ok "version bump: the guard starts the install on its own (no SessionStart)"; else bad "version bump: the guard starts the install on its own (no SessionStart)" "no binary after 10 s; log: $(cat "$c/data/install.log" 2>/dev/null)"; fi
grep -q '  guardrails$' "$c/data/bin/$ver/guardrails.sha256" 2>/dev/null && ok "background install records the sha256" || bad "background install records the sha256" "no record"
i=0; while [ -d "$c/data/bin/.install-lock" ] && [ $i -lt 100 ]; do sleep 0.1; i=$((i + 1)); done
hook pre-tool-use
if ran_stub pre-tool-use && case $out in *"from=old1"*) false ;; *) true ;; esac; then ok "once installed, the guard runs this version's binary"; else bad "once installed, the guard runs this version's binary" "rc=$rc out=$out"; fi
[ ! -d "$c/data/bin/.install-lock" ] && ok "install lock is released" || bad "install lock is released" "still held"
for sub in read-dedup large-read-guard config-gate; do
	newcase "bump-$sub"
	mkid "$c/data/bin/0.0.1/guardrails" old1
	hook "$sub"
	if ran_stub "$sub"; then ok "version bump: $sub runs the older installed binary"; else bad "version bump: $sub runs the older installed binary" "rc=$rc out=$out err=$err"; fi
done

# The newest installed version wins, compared as numbers; prerelease and empty dirs are skipped.
newcase bump-newest
for v in 0.2.0 0.9.0 0.10.0; do mkid "$c/data/bin/$v/guardrails" "v$v"; done
mkid "$c/data/bin/0.11.0-rc1/guardrails" rc
mkdir -p "$c/data/bin/9.9.9"
hook pre-tool-use
case $out in *"from=v0.10.0 "*) ok "version bump: the newest installed version is picked by number" ;; *) bad "version bump: the newest installed version is picked by number" "rc=$rc out=$out err=$err" ;; esac
newcase bump-not-executable
mkdir -p "$c/data/bin/0.0.1"
printf '#!/bin/sh\nexit 3\n' >"$c/data/bin/0.0.1/guardrails"
hook pre-tool-use
expect_blocked "version bump: a non-executable older binary is not used"

# Options still apply to the fallback binary.
newcase bump-observe
mkid "$c/data/bin/0.0.1/guardrails" old1
CLAUDE_PLUGIN_OPTION_OBSERVE=true hook pre-tool-use
case $out in *"observe=true"*) ok "version bump: observe mode reaches the older binary" ;; *) bad "version bump: observe mode reaches the older binary" "$out" ;; esac

# Hooks that are not guards do not start installs.
newcase bump-nonguard
mkrelease "$c/rel"
export CLAUDE_TOOLS_RELEASE_URL=file://$c/rel
hook receipt
sleep 1
[ ! -e "$c/data/bin/$ver/guardrails" ] && [ ! -d "$c/data/bin/.install-lock" ] && ok "no binary: a non-guard hook starts no install" || bad "no binary: a non-guard hook starts no install" "installed or locked"

# A1: config-gate races `install`, and no older binary exists, so it waits for the download.
newcase bump-race
mkrelease "$c/rel"
mkslowcurl "$c/path" 2
export CLAUDE_TOOLS_RELEASE_URL=file://$c/rel
PATH=$c/path:$PATH sh "$run_sh" install </dev/null >"$c/inst.out" 2>&1 &
instpid=$!
sleep 0.5
PATH=$c/path:$PATH hook config-gate
if ran_stub config-gate; then ok "SessionStart race: config-gate waits for the install instead of blocking"; else bad "SessionStart race: config-gate waits for the install instead of blocking" "rc=$rc out=$out err=$err"; fi
wait "$instpid"
[ ! -s "$c/inst.out" ] && ok "SessionStart race: install itself stays silent" || bad "SessionStart race: install itself stays silent" "$(cat "$c/inst.out")"

# A2 with nothing older installed: the guard starts the install and waits for it.
newcase bump-fresh-reload
mkrelease "$c/rel"
mkslowcurl "$c/path" 1
export CLAUDE_TOOLS_RELEASE_URL=file://$c/rel
PATH=$c/path:$PATH hook pre-tool-use
if ran_stub pre-tool-use; then ok "no binary at all: pre-tool-use installs and then runs the guard"; else bad "no binary at all: pre-tool-use installs and then runs the guard" "rc=$rc out=$out err=$err"; fi

# With no binary at all and a failing download the guard still blocks, says the install failed,
# and does not retry the download for every tool call.
newcase bump-fresh-fails
mkslowcurl "$c/path" 0
PATH=$c/path:$PATH hook pre-tool-use
expect_blocked "no binary at all, download fails: pre-tool-use blocks"
case $err in *"last install failed"*) ok "failed install: the message says the install failed" ;; *) bad "failed install: the message says the install failed" "$err" ;; esac
[ -e "$c/data/bin/.fail-$ver" ] && ok "failed install leaves a stamp" || bad "failed install leaves a stamp" "no stamp"
tries=$(grep -c 'tar.gz' "$c/path/curl.log")
PATH=$c/path:$PATH hook pre-tool-use
expect_blocked "failed install: the next call blocks at once"
[ "$(grep -c 'tar.gz' "$c/path/curl.log")" = "$tries" ] && ok "failed install: the next call does not download again" || bad "failed install: the next call does not download again" "downloads before $tries, after $(grep -c 'tar.gz' "$c/path/curl.log")"
PATH=$c/path:$PATH hook install
[ "$(grep -c 'tar.gz' "$c/path/curl.log")" -gt "$tries" ] && ok "failed install: run.sh install always retries" || bad "failed install: run.sh install always retries" "no download"

# An install in flight that does not finish in time: the guard blocks and says it is installing.
newcase bump-inflight
sleep 30 &
holder=$!
mkdir -p "$c/data/bin/.install-lock"
echo "$holder" >"$c/data/bin/.install-lock/pid"
GUARDRAILS_INSTALL_WAIT=1 hook pre-tool-use
kill "$holder" 2>/dev/null
expect_blocked "install in flight past the wait: pre-tool-use blocks"
case $err in *"Installing it in the background"*) ok "install in flight: the message says it is installing" ;; *) bad "install in flight: the message says it is installing" "$err" ;; esac

# fail_open still allows silently, and still starts the install.
newcase bump-failopen
mkrelease "$c/rel"
export CLAUDE_TOOLS_RELEASE_URL=file://$c/rel
CLAUDE_PLUGIN_OPTION_FAIL_OPEN=true hook pre-tool-use
expect_silent_allow "no binary + fail_open: allows silently"
waitfor "$c/data/bin/$ver/guardrails"
[ -x "$c/data/bin/$ver/guardrails" ] && ok "no binary + fail_open: the install still starts" || bad "no binary + fail_open: the install still starts" "no binary"

# Two installers at once download once and leave one intact binary.
newcase concurrent-install
mkrelease "$c/rel"
mkslowcurl "$c/path" 1
export CLAUDE_TOOLS_RELEASE_URL=file://$c/rel
PATH=$c/path:$PATH sh "$run_sh" install </dev/null >"$c/o1" 2>&1 &
p1=$!
PATH=$c/path:$PATH sh "$run_sh" install </dev/null >"$c/o2" 2>&1 &
p2=$!
wait "$p1" "$p2"
[ "$(grep -c 'tar.gz' "$c/path/curl.log")" = 1 ] && ok "two concurrent installs download once" || bad "two concurrent installs download once" "downloads: $(grep -c 'tar.gz' "$c/path/curl.log")"
[ ! -s "$c/o1" ] && [ ! -s "$c/o2" ] && [ -x "$c/data/bin/$ver/guardrails" ] && ok "two concurrent installs both succeed silently" || bad "two concurrent installs both succeed silently" "o1=$(cat "$c/o1") o2=$(cat "$c/o2")"
(cd "$c/data/bin/$ver" && printf '%s\n' "$($sum guardrails | cut -d' ' -f1)  guardrails" | cmp -s - guardrails.sha256) && ok "the sha256 record matches the installed binary" || bad "the sha256 record matches the installed binary" "$(cat "$c/data/bin/$ver/guardrails.sha256")"
ls -A "$c/data/bin" | grep -q '^\.download\.' && bad "no download temp dirs left behind" "$(ls -A "$c/data/bin")" || ok "no download temp dirs left behind"

# A lock whose process is gone does not hold anything up.
newcase dead-lock
mkrelease "$c/rel"
export CLAUDE_TOOLS_RELEASE_URL=file://$c/rel
mkdir -p "$c/data/bin/.install-lock"
sh -c 'echo $$' >"$c/data/bin/.install-lock/pid"
hook install
[ "$rc" = 0 ] && [ -x "$c/data/bin/$ver/guardrails" ] && ok "a lock held by a dead process is taken over" || bad "a lock held by a dead process is taken over" "rc=$rc out=$out"

# Let any detached installer finish before the sandbox is removed.
w=0
while [ -n "$(find "$tmp" -name .install-lock 2>/dev/null)" ] && [ "$w" -lt 100 ]; do sleep 0.1; w=$((w + 1)); done

# Verifier findings. A non-guard hook never runs an older binary: a release can add a hook
# subcommand the older binary rejects with exit 2, which at Stop keeps Claude from ending its turn.
newcase bump-nonguard
mkid "$c/data/bin/0.0.1/guardrails" old1
for sub in brand-new-stop-hook session-env compact-guard; do
	hook "$sub"
	if [ "$rc" = 0 ] && [ -z "$out" ] && [ -z "$err" ]; then ok "version bump: non-guard $sub exits 0 silently and skips the older binary"; else bad "version bump: non-guard $sub exits 0 silently and skips the older binary" "rc=$rc out=$out err=$err"; fi
done

# A directory named guardrails is not a binary.
newcase bump-dir
mkdir -p "$c/data/bin/0.0.1/guardrails"
export GUARDRAILS_INSTALL_WAIT=2
hook pre-tool-use
unset GUARDRAILS_INSTALL_WAIT
if [ "$rc" = 2 ]; then ok "version bump: a directory is not used as the older binary"; else bad "version bump: a directory is not used as the older binary" "rc=$rc out=$out err=$err"; fi

# A guard killed while it waits for the install leaves no copy of the hook JSON in TMPDIR.
newcase wait-kill
mkrelease "$c/rel"
mkslowcurl "$c/path" 5
mkdir -p "$c/tmp"
export CLAUDE_TOOLS_RELEASE_URL=file://$c/rel
TMPDIR=$c/tmp PATH=$c/path:$PATH sh "$run_sh" pre-tool-use <<<'{"tool_name":"Bash"}' >/dev/null 2>&1 &
gp=$!
sleep 1
kill "$gp" 2>/dev/null
wait "$gp" 2>/dev/null
if [ -z "$(ls -A "$c/tmp")" ]; then ok "a guard killed during the wait leaves no temp copy of its input"; else bad "a guard killed during the wait leaves no temp copy of its input" "$(ls -A "$c/tmp")"; fi
pkill -f "$c/rel" 2>/dev/null
sleep 6

# First install: four guards and the SessionStart install race on an empty data dir. None may
# block or say the install failed, and the release is downloaded once.
racefail=
for t in 1 2 3 4 5 6 7 8 9 10 11 12; do
	newcase "race-$t"
	mkrelease "$c/rel"
	mkslowcurl "$c/path" 0
	export CLAUDE_TOOLS_RELEASE_URL=file://$c/rel
	for n in 1 2 3 4; do
		PATH=$c/path:$PATH sh "$run_sh" pre-tool-use <<<'{"tool_name":"Bash"}' >"$c/g$n" 2>"$c/e$n" &
		eval "g$n=\$!"
	done
	PATH=$c/path:$PATH sh "$run_sh" install </dev/null >"$c/i" 2>&1 &
	gi=$!
	wait "$g1" "$g2" "$g3" "$g4" "$gi"
	for n in 1 2 3 4; do
		grep -q BLOCKED "$c/e$n" && racefail="$racefail race-$t:guard$n-blocked"
	done
	[ "$(grep -c 'tar.gz' "$c/path/curl.log")" = 1 ] || racefail="$racefail race-$t:downloads=$(grep -c 'tar.gz' "$c/path/curl.log")"
done
if [ -z "$racefail" ]; then ok "first-install race: no guard blocks and the release downloads once (12 trials)"; else bad "first-install race: no guard blocks and the release downloads once (12 trials)" "$racefail"; fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" = 0 ]
