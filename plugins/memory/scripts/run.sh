#!/bin/sh
# memory hook launcher. Unlike guardrails, this plugin FAILS OPEN: whatever goes wrong (no
# binary, failed download, checksum mismatch, unsupported platform, crashing or hung binary,
# daemon down) it exits 0 and prints nothing to stdout, so the session continues without
# memory. Diagnostics go to $data/memory.log. A binary whose checksum does not match is never
# installed and never run.
#
#   run.sh session-start   install the binary if needed, start the daemon, print the context block
#   run.sh install         install the binary if needed
#   run.sh mcp             exec the MCP stdio server (exits 0 at once if there is no binary)
#   run.sh <subcommand>    run `memory <subcommand>` with the hook's stdin, bounded by a timeout
#
# Test seams: MEMORY_BIN (binary path, skips install), MEMORY_RELEASE_URL (download base),
# MEMORY_HOOK_TIMEOUT (seconds, default 3), MEMORY_NO_DAEMON=1 (do not start the daemon).
root=${CLAUDE_PLUGIN_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}
data=${CLAUDE_PLUGIN_DATA:-$HOME/.claude/plugins/data/memory}
ver=$(sed -n 's/^ *"version": *"\([^"]*\)".*/\1/p' "$root/.claude-plugin/plugin.json" 2>/dev/null)
dest=$data/bin/$ver/memory
bin=${MEMORY_BIN:-$dest}
limit=${MEMORY_HOOK_TIMEOUT:-3}
log=$data/memory.log
mkdir -p "$data" 2>/dev/null
# The subshell matters: a failed redirect on the `:` special builtin aborts a POSIX shell.
( : >>"$log" ) 2>/dev/null || log=/dev/null

log() {
	if [ "$log" = /dev/null ]; then
		printf 'memory: %s\n' "$*" >&2
	else
		printf '%s memory: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >>"$log"
	fi
}

# The binary's state directory and the daemon socket directory. The socket needs a runtime dir;
# systemd sets one on Linux, macOS has none, so fall back to a private per-user temp dir.
export MEMORY_DIR="${MEMORY_DIR:-$HOME/.broodhall/memory}"
if [ -z "${XDG_RUNTIME_DIR-}" ]; then
	XDG_RUNTIME_DIR=${TMPDIR:-/tmp}/memory-$(id -u)
	mkdir -p "$XDG_RUNTIME_DIR" 2>/dev/null && chmod 700 "$XDG_RUNTIME_DIR" 2>/dev/null
	export XDG_RUNTIME_DIR
fi

# install_bin downloads, verifies and unpacks the release binary. It runs in a subshell
# (the parentheses) so its cleanup trap and exits stay local. A failure leaves a stamp so a
# machine without network does not pay the download timeout at every session start.
install_bin() (
	[ -x "$bin" ] && exit 0
	fail() {
		log "install of memory $ver failed: $1; memory is off this session"
		touch "$data/bin/.fail-$ver" 2>/dev/null
		exit 1
	}
	[ -n "${MEMORY_BIN-}" ] && { log "MEMORY_BIN=$MEMORY_BIN is not executable"; exit 1; }
	[ -n "$(find "$data/bin/.fail-$ver" -mmin -60 2>/dev/null)" ] && { log "install of $ver failed within the last hour; not retrying"; exit 1; }
	os=$(uname -s | tr '[:upper:]' '[:lower:]')
	case $os in linux | darwin) ;; *) fail "unsupported OS $os" ;; esac
	case $(uname -m) in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) fail "unsupported CPU $(uname -m)" ;;
	esac
	command -v curl >/dev/null 2>&1 || fail "curl not found"
	asset=memory_${os}_$arch.tar.gz
	base=${MEMORY_RELEASE_URL:-https://github.com/jj-scoll/broodhall/releases/download}
	mkdir -p "$data/bin" && tmp=$(mktemp -d "$data/bin/.download.XXXXXX") || fail "cannot write $data"
	trap 'rm -rf "$tmp"' EXIT
	curl -fsSL --connect-timeout 5 --max-time 60 -o "$tmp/$asset" "$base/memory-v$ver/$asset" &&
		curl -fsSL --connect-timeout 5 --max-time 30 -o "$tmp/checksums.txt" "$base/memory-v$ver/checksums.txt" ||
		fail "download of $asset failed"
	if command -v sha256sum >/dev/null 2>&1; then sum=sha256sum; else sum='shasum -a 256'; fi
	(cd "$tmp" && grep "  $asset\$" checksums.txt | $sum -c - >/dev/null 2>&1) || fail "checksum mismatch or missing for $asset"
	tar -xzf "$tmp/$asset" -C "$tmp" memory && mkdir -p "${dest%/*}" &&
		mv -f "$tmp/memory" "$dest" || fail "unpack failed"
	exit 0
)

# start_daemon launches `memory serve` detached, with no inherited stdio so the hook's pipes
# close. A second instance exits at once because the first holds the socket.
start_daemon() {
	[ -n "${MEMORY_NO_DAEMON-}" ] && return 0
	if command -v setsid >/dev/null 2>&1; then
		setsid "$bin" serve >>"$log" 2>&1 </dev/null &
	else
		nohup "$bin" serve >>"$log" 2>&1 </dev/null &
	fi
}

# run_bounded runs the binary with stdin from $in and stdout to $outf. A watchdog kills it
# after $limit seconds. Returns the binary's status (nonzero when it was killed).
run_bounded() {
	"$bin" "$@" <"$in" >"$outf" 2>>"$log" &
	pid=$!
	(
		exec >/dev/null 2>&1 </dev/null
		sleep "$limit"
		kill -9 "$pid"
	) &
	wd=$!
	wait "$pid"
	rc=$?
	kill "$wd" 2>/dev/null
	wait "$wd" 2>/dev/null
	[ "$rc" -eq 0 ] || log "memory $* exited $rc (137 means the $limit s timeout killed it)"
	return "$rc"
}

# emit prints the binary's stdout only when it exited 0, so a crash or a kill never leaks a
# partial or half-formed hook decision.
emit() {
	if run_bounded "$@"; then cat "$outf"; fi
}

main() {
	mode=${1-}
	if [ "$mode" = mcp ]; then
		[ -x "$bin" ] && exec "$bin" mcp
		log "mcp: no binary at $bin"
		return 0
	fi
	tmpd=$(mktemp -d "${TMPDIR:-/tmp}/memory-hook.XXXXXX" 2>/dev/null) || { log "no temp dir"; cat >/dev/null; return 0; }
	trap 'rm -rf "$tmpd"' EXIT
	in=$tmpd/in outf=$tmpd/out
	cat >"$in" 2>/dev/null
	case $mode in
	install) install_bin ;;
	session-start)
		install_bin || return 0
		start_daemon
		emit context
		;;
	"") log "usage: run.sh session-start|install|mcp|<subcommand>" ;;
	*)
		if [ -x "$bin" ]; then emit "$@"; else log "no binary at $bin"; fi
		;;
	esac
	return 0
}

main "$@"
exit 0
