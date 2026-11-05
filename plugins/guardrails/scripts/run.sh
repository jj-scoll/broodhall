#!/bin/sh
# guardrails hook launcher: runs this plugin version's guardrails binary on the hook's stdin.
# `run.sh install` (the SessionStart hook) downloads the binary from the GitHub release the
# first time a version runs, records its sha256 next to it, then runs `guardrails doctor
# -hook`, which says nothing unless the install has a problem. A failed install or a failed
# doctor never stops the session start. The guards
# (pre-tool-use, read-dedup, large-read-guard, config-gate) fail CLOSED: with no binary they exit
# 2 and say on stderr what failed, how to retry and how to opt out, so PreToolUse blocks the
# tool call instead of letting it through unguarded. Every other hook exits 0 silently, since
# exit 2 at Stop would keep Claude from ending its turn and at PreCompact would block compaction.
# The opt-out is the fail_open option (CLAUDE_PLUGIN_OPTION_FAIL_OPEN) or, outside the plugin,
# GUARDRAILS_FAIL_OPEN: either one set to true, 1 or yes makes a missing binary exit 0 silently.
# The binary was named claude-tools before the rename. Every GUARDRAILS_<NAME> variable falls
# back to CLAUDE_TOOLS_<NAME> when unset. Each plugin version downloads its own release, so
# older versions still fetch claude-tools_<os>_<arch>.tar.gz from their own tags.
#
# A version bump. Claude Code runs a hook event's commands in parallel, and /reload-plugins
# switches to a new version without a SessionStart, so a guard can run before this version's
# binary is downloaded. It then does not block just for that: it runs the newest binary already
# installed for another version (any bin/<x.y.z>/guardrails or claude-tools that is executable)
# and starts this version's install in the background. With no binary at all (a first install)
# it starts the install and waits up to GUARDRAILS_INSTALL_WAIT seconds (default 25) for it,
# then blocks as before if it did not land.
# shellcheck disable=SC2015 # `a && b || fail`: fail when any step fails, which is intended
set -u
root=${CLAUDE_PLUGIN_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}
ver=$(sed -n 's/^ *"version": *"\([^"]*\)".*/\1/p' "$root/.claude-plugin/plugin.json")
# Claude Code sets CLAUDE_PLUGIN_DATA for hooks. Run from a terminal, the launcher finds that dir
# itself: Claude Code caches the plugin at <config>/plugins/cache/<marketplace>/guardrails/
# <version> and keeps its data in <config>/plugins/data/guardrails-<marketplace>. Outside that
# cache (a checkout) it takes the guardrails-* data dir that holds this version's binary, else
# guardrails.
data=${CLAUDE_PLUGIN_DATA-}
if [ -z "$data" ]; then
	mkt=${root%/*/*}
	case ${mkt%/*} in
	*/plugins/cache) data=${mkt%/cache/*}/data/guardrails-${mkt##*/} ;;
	*)
		cfg=${CLAUDE_CONFIG_DIR:-$HOME/.claude}
		data=$cfg/plugins/data/guardrails
		for d in "$cfg"/plugins/data/guardrails-*; do
			[ -x "$d/bin/$ver/guardrails" ] || [ -x "$d/bin/$ver/claude-tools" ] && data=$d
		done
		;;
	esac
fi
# Main can advertise a version whose release predates the rename until the next release ships:
# the launcher before the rename installed that version's binary as claude-tools, so use it.
pick_dest() {
	dest=$data/bin/$ver/guardrails
	[ -x "$dest" ] || [ ! -x "${dest%/*}/claude-tools" ] || dest=${dest%/*}/claude-tools
	curbin=${GUARDRAILS_BIN:-${CLAUDE_TOOLS_BIN:-$dest}}
}
pick_dest

export CLAUDE_PLUGIN_ROOT="$root"
# A detached installer finds the same data dir, however this run found it.
export CLAUDE_PLUGIN_DATA="$data"

# setting <NAME> <OPTION>: the userConfig option (CLAUDE_PLUGIN_OPTION_<OPTION>) when the user
# set it, else GUARDRAILS_<NAME>, else CLAUDE_TOOLS_<NAME>, else empty (plugin.json's
# default). It is exported under both names: a binary from before the rename reads only
# CLAUDE_TOOLS_<NAME>, and main can run one until the next release ships. The eval only
# builds the variable names from these constant arguments; values are never re-parsed.
setting() {
	v=
	eval "v=\${CLAUDE_PLUGIN_OPTION_$2-\${GUARDRAILS_$1-\${CLAUDE_TOOLS_$1-}}}"
	export "GUARDRAILS_$1=$v" "CLAUDE_TOOLS_$1=$v"
}
setting OFF DISABLED_GUARDS
setting PII_ALLOW ALLOWED_EMAIL_DOMAINS
# Client-data rules come from the user; with none set they do nothing (see docs/guardrails/pii.md).
setting CLIENT_BUCKETS CLIENT_DATA_BUCKETS
setting MASKED_DIRS MASKED_COPY_DIRS
export HANDOFF_TOKENS="${CLAUDE_PLUGIN_OPTION_HANDOFF_TOKENS-${HANDOFF_TOKENS-}}"
# Observe mode (log only) is off unless the observe option (or GUARDRAILS_OBSERVE) says on.
setting OBSERVE OBSERVE
# The opt-in rtk rewrite is off unless the rtk option (or GUARDRAILS_RTK) says on.
setting RTK RTK
setting RTK_CMD RTK_COMMAND
setting FAIL_OPEN FAIL_OPEN
# Per-session guard state stays with this user's plugin data, not in a shared /tmp dir.
export HANDOFF_STATE_DIR="${HANDOFF_STATE_DIR-$data}"

case $GUARDRAILS_FAIL_OPEN in
true | TRUE | True | 1 | yes) fail_open=1 ;;
*) fail_open= ;;
esac

# One installer at a time: the lock dir $data/bin/.install-lock belongs to the process whose pid
# is in $lock/pid (an empty pid file counts as held). A lock whose process is gone, or that is
# older than 15 minutes, is taken over. A failed install leaves a stamp, and the guards
# do not start another install for 5 minutes (`run.sh install` always tries).
lock=$data/bin/.install-lock
stamp=$data/bin/.fail-$ver
log=$data/install.log
wait_s=${GUARDRAILS_INSTALL_WAIT:-25}

lock_live() {
	p=$(cat "$lock/pid" 2>/dev/null) # take_lock checks it again before it takes the lock over
	[ -d "$lock" ] || return 1
	# Another process is taking it over (see take_lock): wait for that one as for an installer.
	[ -e "$lock/takeover" ] && [ -z "$(find "$lock/takeover" -mmin +1 2>/dev/null)" ] && return 0
	[ -z "$(find "$lock" -maxdepth 0 -mmin +15 2>/dev/null)" ] || return 1
	# No pid yet: its owner is between mkdir and writing it, or died there. Give it a minute.
	if [ -z "$p" ]; then
		[ -z "$(find "$lock" -maxdepth 0 -mmin +1 2>/dev/null)" ]
		return
	fi
	kill -0 "$p" 2>/dev/null
}
# take_lock makes the lock ours. The pid file is the claim: set -C creates it with O_EXCL, which
# the kernel makes exclusive. mkdir is not: uutils mkdir (Ubuntu 25.10 and later) can let two
# racing processes both succeed. A dead or expired lock is moved aside and removed first.
# Two takers can both find it dead, and the slower one would then move aside the lock the faster
# one had just made and claimed. So a taker first creates $lock/takeover (set -C again: one wins),
# and moves the lock aside only if its pid file still holds the pid lock_live judged. The token
# goes with the lock it was made in; one older than a minute was left by a taker that died.
claim_lock() { (set -C && echo $$ >"$lock/pid") 2>/dev/null; }
take_lock() {
	mkdir -p "$lock" 2>/dev/null || return 1
	claim_lock && return 0
	lock_live && return 1
	if (set -C && : >"$lock/takeover") 2>/dev/null || [ -n "$(find "$lock/takeover" -mmin +1 2>/dev/null)" ]; then
		if [ -f "$lock/pid" ] && [ "$(cat "$lock/pid" 2>/dev/null)" = "$p" ]; then
			mv "$lock" "$lock.stale.$$" 2>/dev/null && rm -rf "$lock.stale.$$"
		else
			rm -f "$lock/takeover"
		fi
	fi
	mkdir -p "$lock" 2>/dev/null && claim_lock
}
recent_failure() { [ -n "$(find "$stamp" -mmin -5 2>/dev/null)" ]; }

# say_fail prints the SessionStart notice for a failed install; the install never stops the session.
say_fail() {
	if [ -n "$fail_open" ]; then
		effect="so its guards are off this session (fail_open is set)"
	else
		effect="so tool calls are blocked until it installs (set the fail_open option to allow them unguarded)"
	fi
	printf '{"systemMessage":"guardrails: could not install guardrails %s (%s), %s. It retries at the next session start."}\n' "$ver" "$1" "$effect"
}

# install_bin downloads, verifies and unpacks this version's binary. It runs in a subshell (the
# parentheses) so its cleanup trap and exits stay local; a failure prints say_fail's notice,
# leaves the failure stamp and returns 1.
install_bin() (
	fail() {
		touch "$stamp" 2>/dev/null
		say_fail "$1"
		exit 1
	}
	dest=$data/bin/$ver/guardrails # where an install puts it
	base=${GUARDRAILS_RELEASE_URL:-${CLAUDE_TOOLS_RELEASE_URL:-https://github.com/jj-scoll/broodhall/releases/download}}
	os=$(uname -s | tr '[:upper:]' '[:lower:]')
	case $(uname -m) in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) fail "unsupported CPU $(uname -m)" ;;
	esac
	asset=guardrails_${os}_$arch.tar.gz want=$asset member=guardrails
	command -v curl >/dev/null 2>&1 || fail "curl not found"
	mkdir -p "$data/bin" && tmp=$(mktemp -d "$data/bin/.download.XXXXXX") || fail "cannot write $data"
	trap 'rm -rf "$tmp"' EXIT
	# A release made before the rename has only claude-tools_<os>_<arch>.tar.gz (see pick_dest).
	{ curl -fsSL --max-time 60 -o "$tmp/$asset" "$base/v$ver/$asset" || {
		asset=claude-tools_${os}_$arch.tar.gz member=claude-tools
		curl -fsSL --max-time 60 -o "$tmp/$asset" "$base/v$ver/$asset"
	}; } &&
		curl -fsSL --max-time 30 -o "$tmp/checksums.txt" "$base/v$ver/checksums.txt" ||
		fail "download of $want failed"
	if command -v sha256sum >/dev/null; then sum=sha256sum; else sum='shasum -a 256'; fi
	# Pick the asset's line first: `$sum -c` on empty input exits 0 or errors depending on the
	# shasum build, so an asset missing from checksums.txt is refused here, by name.
	line=$(grep "  $asset\$" "$tmp/checksums.txt")
	[ -n "$line" ] || fail "no checksum for $asset in checksums.txt"
	(cd "$tmp" && printf '%s\n' "$line" | $sum -c - >/dev/null 2>&1) || fail "checksum mismatch"
	tar -xzf "$tmp/$asset" -C "$tmp" "$member" || fail "unpack failed"
	# What doctor compares the binary with later: the unpacked file's hash, taken after the
	# archive passed the release checksum. It is written before the binary moves into place, so
	# a binary that exists always has its record.
	mkdir -p "${dest%/*}" && $sum "$tmp/$member" | sed 's|  .*|  guardrails|' >"$dest.sha256" &&
		mv -f "$tmp/$member" "$dest" || fail "unpack failed"
	rm -f "$stamp"
	exit 0
)

# spawn runs a command detached, with no inherited stdio so the hook's pipes close.
spawn() {
	if command -v setsid >/dev/null 2>&1; then
		setsid "$@" >>"$log" 2>&1 </dev/null &
	else
		nohup "$@" >>"$log" 2>&1 </dev/null &
	fi
}

# start_install makes sure an install of this version is running in the background. It returns 0
# when one is running (started here or already in flight) and 1 when none is: a recent failure,
# or a data dir it cannot write.
start_install() {
	mkdir -p "$data/bin" 2>/dev/null || return 1
	recent_failure && ! lock_live && return 1
	take_lock || return 0 # lost the race to another starter, which is installing
	spawn sh "$0" install-locked
	# The installer holds it now, however soon this guard exits. Unless it has already finished
	# and another process has the lock: then the pid file is not ours to write.
	[ "$(cat "$lock/pid" 2>/dev/null)" = $$ ] && echo $! >"$lock/pid" 2>/dev/null
	return 0
}

# wait_install waits up to $wait_s seconds for the running install to finish.
wait_install() {
	n=$wait_s
	while [ "$n" -gt 0 ] && lock_live; do
		sleep 1
		n=$((n - 1))
	done
	pick_dest
}

case ${1-} in
install-locked) # the detached child of start_install, which took the lock
	echo $$ >"$lock/pid" 2>/dev/null
	# Another install can land between the guard's binary check and its lock: check again.
	pick_dest
	[ -x "$curbin" ] || install_bin
	rm -rf "$lock"
	exit 0
	;;
install)
	doctor() { "$curbin" doctor -hook || true; exit 0; }
	[ -x "$curbin" ] && doctor
	# Checked before the lock, which would otherwise report an unwritable dir as a running install.
	{ mkdir -p "$data/bin" && [ -w "$data/bin" ]; } 2>/dev/null || { say_fail "cannot write $data"; exit 0; }
	if ! take_lock; then # another installer is running: wait for it instead of racing it
		n=90
		while [ "$n" -gt 0 ] && lock_live; do
			sleep 1
			n=$((n - 1))
		done
		pick_dest
		[ -x "$curbin" ] && doctor
		take_lock || { say_fail "another install is running"; exit 0; }
	fi
	echo $$ >"$lock/pid" 2>/dev/null
	trap 'rm -rf "$lock"' EXIT
	# Another installer can finish between the check above and this lock: check again.
	pick_dest
	[ -x "$curbin" ] && doctor
	install_bin || exit 0
	pick_dest
	doctor
	;;
esac

# newest_installed prints the highest-numbered x.y.z version's binary, other than this
# version's, that is executable. Prerelease-style directory names are skipped.
newest_installed() {
	for d in "$data"/bin/[0-9]*.[0-9]*.[0-9]*; do
		v=${d##*/}
		[ "$v" = "$ver" ] && continue
		case $v in *[!0-9.]*) continue ;; esac
		for n in guardrails claude-tools; do
			if [ -f "$d/$n" ] && [ -x "$d/$n" ]; then
				printf '%s %s\n' "$v" "$d/$n"
				break
			fi
		done
	done | sort -t. -k1,1n -k2,2n -k3,3n | tail -n 1 | cut -d' ' -f2-
}

guard=
case ${1-} in pre-tool-use | read-dedup | large-read-guard | config-gate) guard=1 ;; esac

bin=$curbin
state=failed # installing: an install is running and may land; failed: none is
# Only guards fall back: a release can add a hook subcommand the older binary rejects with
# exit 2, which at Stop would keep Claude from ending its turn.
if [ -n "$guard" ] && [ ! -x "$bin" ] && [ -z "${GUARDRAILS_BIN:-${CLAUDE_TOOLS_BIN-}}" ]; then
	older=$(newest_installed)
	if [ -n "$older" ]; then
		bin=$older
		start_install
	else
		start_install && state=installing
	fi
fi

if [ ! -x "$bin" ]; then
	if [ -z "$guard" ] || [ -n "$fail_open" ]; then
		cat >/dev/null
		exit 0
	fi
	if [ "$state" = installing ] && input=$(mktemp "${TMPDIR:-/tmp}/guardrails-in.XXXXXX"); then
		trap 'rm -f "$input"' EXIT
		trap 'exit 143' HUP INT TERM
		cat >"$input"
		wait_install
		if [ -x "$curbin" ]; then
			"$curbin" "$@" <"$input"
			rc=$?
			rm -f "$input"
			exit $rc
		fi
		rm -f "$input"
		! recent_failure || state=failed
	else
		cat >/dev/null
		[ "$state" = installing ] && wait_install
	fi
	{
		printf 'guardrails: BLOCKED: the guard binary guardrails %s is not installed (%s), so this tool call was not checked.\n' "$ver" "$curbin"
		if [ "$state" = installing ]; then
			printf 'Installing it in the background now: run the tool call again in a minute. To install by hand: sh "%s/scripts/run.sh" install\n' "$root"
		else
			printf 'The last install failed. Retry now with: sh "%s/scripts/run.sh" install (it says why it failed)\n' "$root"
		fi
		printf 'Opt out: set the guardrails option fail_open to true (or GUARDRAILS_FAIL_OPEN=1) to allow tool calls without guards.\n'
	} >&2
	exit 2
fi
exec "$bin" "$@"
