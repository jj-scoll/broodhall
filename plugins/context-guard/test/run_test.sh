#!/bin/bash
# Tests for plugins/context-guard/scripts/run.sh, the launcher the mod calls to reach the
# claude-tools binary the guardrails plugin installed. Run: bash plugins/context-guard/test/run_test.sh
#
# Contract under test: `run.sh state` prints the state base dir guardrails' hooks use, and
# `run.sh autoclear ...` runs `claude-tools autoclear ...` with that dir. With no binary or
# state dir the launcher prints nothing and exits 1, so the mod skips the clear.
set -u
here=$(cd "$(dirname "$0")" && pwd)
run_sh=$(cd "$here/.." && pwd)/scripts/run.sh

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
pass=0 fail=0

ok() { pass=$((pass + 1)); printf 'ok   %s\n' "$1"; }
bad() { fail=$((fail + 1)); printf 'FAIL %s: %s\n' "$1" "$2"; }
eq() { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1" "got [$2], want [$3]"; fi; }

# A stub claude-tools that records its arguments and the state dir it was given.
mkstub() {
	mkdir -p "$(dirname "$1")"
	cat >"$1" <<'EOF'
#!/bin/sh
echo "args=$* state=${HANDOFF_STATE_DIR-unset}"
EOF
	chmod +x "$1"
}

# fresh <name>: a config dir with no guardrails data; sets cfg and the env the launcher reads.
fresh() {
	cfg=$tmp/$1
	mkdir -p "$cfg"
	export CLAUDE_CONFIG_DIR=$cfg
	unset HANDOFF_STATE_DIR CLAUDE_TOOLS_BIN
}
data() { printf '%s/plugins/data/%s' "$cfg" "$1"; }

fresh none
out=$(sh "$run_sh" state); rc=$?
eq "no guardrails: state prints nothing" "$out" ""
eq "no guardrails: state exits 1" "$rc" "1"
out=$(sh "$run_sh" autoclear --claim s1); rc=$?
eq "no guardrails: autoclear prints nothing" "$out" ""
eq "no guardrails: autoclear exits 1" "$rc" "1"

fresh found
mkstub "$(data guardrails-broodhall)/bin/0.3.0/claude-tools"
eq "state is the guardrails data dir" "$(sh "$run_sh" state)" "$(data guardrails-broodhall)"
eq "autoclear runs the binary with the guardrails data dir as state" \
	"$(sh "$run_sh" autoclear --claim s1)" "args=autoclear --claim s1 state=$(data guardrails-broodhall)"
eq "arguments with spaces survive" \
	"$(sh "$run_sh" autoclear --done s1 ok 'cleared into s2')" \
	"args=autoclear --done s1 ok cleared into s2 state=$(data guardrails-broodhall)"

fresh newest
mkstub "$(data guardrails-inline)/bin/0.2.0/claude-tools"
touch -d '-1 hour' "$(data guardrails-inline)/bin/0.2.0/claude-tools"
mkstub "$(data guardrails-broodhall)/bin/0.3.0/claude-tools"
eq "the most recently installed binary wins" "$(sh "$run_sh" state)" "$(data guardrails-broodhall)"
touch -d '-2 hours' "$(data guardrails-broodhall)/bin/0.3.0/claude-tools"
eq "...by modification time, not by name" "$(sh "$run_sh" state)" "$(data guardrails-inline)"

fresh overrides
mkstub "$(data guardrails-broodhall)/bin/0.3.0/claude-tools"
eq "HANDOFF_STATE_DIR wins, as in guardrails' launcher" \
	"$(HANDOFF_STATE_DIR=$tmp/mystate sh "$run_sh" autoclear --claim s1)" \
	"args=autoclear --claim s1 state=$tmp/mystate"
eq "state prints HANDOFF_STATE_DIR" "$(HANDOFF_STATE_DIR=$tmp/mystate sh "$run_sh" state)" "$tmp/mystate"
eq "an empty HANDOFF_STATE_DIR means /tmp, as in the binary" "$(HANDOFF_STATE_DIR='' sh "$run_sh" state)" "/tmp"
mkstub "$tmp/mybuild/claude-tools"
eq "CLAUDE_TOOLS_BIN runs that binary" \
	"$(CLAUDE_TOOLS_BIN=$tmp/mybuild/claude-tools sh "$run_sh" autoclear --claim s1)" \
	"args=autoclear --claim s1 state=$(data guardrails-broodhall)"
eq "a CLAUDE_TOOLS_BIN that is not executable gives nothing" \
	"$(CLAUDE_TOOLS_BIN=$tmp/nope sh "$run_sh" autoclear --claim s1)" ""

fresh nostate
mkstub "$tmp/mybuild/claude-tools"
out=$(CLAUDE_TOOLS_BIN=$tmp/mybuild/claude-tools sh "$run_sh" state); rc=$?
eq "a binary but no guardrails data dir and no HANDOFF_STATE_DIR: no state" "$out|$rc" "|1"

fresh stray
mkdir -p "$(data guardrails-broodhall)/bin/0.3.0"
out=$(sh "$run_sh" state); rc=$?
eq "a version dir without a binary is not a binary" "$out|$rc" "|1"

fresh unknown
mkstub "$(data guardrails-broodhall)/bin/0.3.0/claude-tools"
out=$(sh "$run_sh" rm -rf /); rc=$?
eq "only state and autoclear are served" "$out|$rc" "|2"

printf '%s passed, %s failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
