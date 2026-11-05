#!/bin/sh
# memory hook launcher. Unlike guardrails, this plugin FAILS OPEN: whatever goes wrong (no
# binary, failed download, checksum mismatch, unsupported platform, crashing or hung binary,
# daemon down) it exits 0 and prints nothing to stdout, so the session continues without
# memory. Diagnostics go to $data/memory.log. A binary whose checksum does not match is never
# installed and never run.
#
#   run.sh session-start   binary installed: start the daemon, print the context block;
#                          not installed: start a background install and print nothing
#   run.sh install         install the binary if needed (in the foreground)
#   run.sh mcp             exec the MCP stdio server; with no binary for this version it serves from the
#                          newest installed older one, or waits (bounded) for the install, else exits 0
#   run.sh <subcommand>    run `memory <subcommand>` with the hook's stdin, bounded by a timeout
#
# Test seams: MEMORY_BIN (binary path, skips install), MEMORY_RELEASE_URL (download base),
# MEMORY_HOOK_TIMEOUT (seconds, default 3), MEMORY_NO_DAEMON=1 (do not start the daemon).
root=${CLAUDE_PLUGIN_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}
ver=$(sed -n 's/^ *"version": *"\([^"]*\)".*/\1/p' "$root/.claude-plugin/plugin.json" 2>/dev/null)
# Claude Code sets CLAUDE_PLUGIN_DATA for hooks and MCP. Run from a terminal, the launcher finds
# that dir itself: Claude Code caches the plugin at <config>/plugins/cache/<marketplace>/memory/
# <version> and keeps its data in <config>/plugins/data/memory-<marketplace>. Outside that cache
# (a checkout) it takes the memory-* data dir that holds this version's binary, else memory.
data=${CLAUDE_PLUGIN_DATA-}
if [ -z "$data" ]; then
	mkt=${root%/*/*}
	case ${mkt%/*} in
	*/plugins/cache) data=${mkt%/cache/*}/data/memory-${mkt##*/} ;;
	*)
		cfg=${CLAUDE_CONFIG_DIR:-$HOME/.claude}
		data=$cfg/plugins/data/memory
		for d in "$cfg"/plugins/data/memory-*; do
			[ -x "$d/bin/$ver/memory" ] && data=$d
		done
		;;
	esac
fi
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

# The installed binary loads the DuckDB extensions shipped in its own tarball; MEMORY_BIN (a
# local build) keeps the binary's default. The state dir is the binary's choice (MEMORY_DIR,
# else $XDG_DATA_HOME/memory, else ~/.local/share/memory). The daemon socket needs a runtime
# dir; systemd sets one on Linux, macOS has none, so fall back to a private per-user temp dir.
[ -z "${MEMORY_BIN-}" ] && export MEMORY_EXT_DIR="${MEMORY_EXT_DIR:-${dest%/*}/ext}"
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
	# GNU tar unpacks .tar.xz through the xz program; macOS tar reads it natively.
	[ "$os" = darwin ] || command -v xz >/dev/null 2>&1 || fail "xz not found (install xz-utils)"
	asset=memory_${os}_$arch.tar.xz
	base=${MEMORY_RELEASE_URL:-https://github.com/jj-scoll/broodhall/releases/download}
	if ! { mkdir -p "$data/bin" && tmp=$(mktemp -d "$data/bin/.download.XXXXXX"); }; then fail "cannot write $data"; fi
	trap 'rm -rf "$tmp"' EXIT
	# The install runs in the background (see bg_install), so a slow link only needs to finish
	# within 10 minutes.
	if ! { curl -fsSL --connect-timeout 5 --max-time 600 -o "$tmp/$asset" "$base/v$ver/$asset" &&
		curl -fsSL --connect-timeout 5 --max-time 30 -o "$tmp/checksums.txt" "$base/v$ver/checksums.txt"; }; then
		fail "download of $asset failed"
	fi
	if command -v sha256sum >/dev/null 2>&1; then sum=sha256sum; else sum='shasum -a 256'; fi
	# Compare the hashes directly: macOS shasum -c exits 0 on empty input, so piping a missing
	# checksums.txt line into it would pass an unlisted asset.
	want=$(grep "  $asset\$" "$tmp/checksums.txt" | cut -d' ' -f1)
	got=$($sum "$tmp/$asset" | cut -d' ' -f1)
	[ "${#want}" -eq 64 ] || fail "checksums.txt has no entry for $asset"
	[ "$got" = "$want" ] || fail "checksum mismatch for $asset"
	# The extensions go in first and the binary last, so an installed binary always has them.
	tar -xJf "$tmp/$asset" -C "$tmp" memory ext >/dev/null 2>&1 || fail "unpack of $asset failed (it must hold memory and ext/)"
	for e in fts httpfs quack; do
		[ -f "$tmp/ext/$e.duckdb_extension" ] || fail "$asset has no ext/$e.duckdb_extension"
	done
	if ! { mkdir -p "${dest%/*}" && rm -rf "${dest%/*}/ext" && mv -f "$tmp/ext" "${dest%/*}/ext" &&
		mv -f "$tmp/memory" "$dest"; }; then
		fail "install into ${dest%/*} failed"
	fi
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

# start_daemon launches `memory serve` detached. A second instance exits at once because the
# first holds the socket.
start_daemon() {
	[ -n "${MEMORY_NO_DAEMON-}" ] && return 0
	spawn "$bin" serve
}

# bg_install starts the install detached and returns at once, so a slow download never holds
# up a session; memory starts once it lands. The lock keeps two sessions from installing at the
# same time. Its claim file is what makes it exclusive: set -C creates it with O_EXCL, which the
# kernel makes exclusive. mkdir is not: uutils mkdir (Ubuntu 25.10 and later) can let two racing
# processes both succeed. A lock older than 15 minutes belongs to a killed installer and is taken over.
lock=$data/bin/.install-lock
bg_install() {
	mkdir -p "$data/bin" 2>/dev/null || { log "cannot write $data/bin; memory is off this session"; return 0; }
	[ -n "$(find "$lock" -maxdepth 0 -mmin +15 2>/dev/null)" ] && rm -rf "$lock"
	{ mkdir -p "$lock" && (set -C && echo $$ >"$lock/claim"); } 2>/dev/null || return 0
	spawn sh "$0" install-locked
}

# newest_installed prints the highest-numbered x.y.z version's memory binary, other than this
# version's, that is executable. Prerelease-style directory names are skipped.
newest_installed() {
	for d in "$data"/bin/[0-9]*.[0-9]*.[0-9]*; do
		v=${d##*/}
		[ "$v" = "$ver" ] && continue
		case $v in *[!0-9.]*) continue ;; esac
		[ -x "$d/memory" ] && printf '%s %s\n' "$v" "$d/memory"
	done | sort -t. -k1,1n -k2,2n -k3,3n | tail -n 1 | cut -d' ' -f2-
}

# mcp_without_binary runs when this version's binary is not installed. Claude Code caches an
# MCP server that exits at startup for about 15 minutes, so exiting here would cost memory's
# tools for the whole session even though the install lands seconds later. Instead it starts
# the install (if none is running) and then serves from the newest binary of another version
# (mcp only forwards each request to the daemon's socket, so a version difference is safe), or,
# with none installed, waits up to MEMORY_MCP_WAIT seconds (default 25, under Claude Code's
# 30 s connect timeout) for the install. It returns, and the launcher exits 0, only if nothing
# could serve.
mcp_without_binary() {
	if [ -n "${MEMORY_BIN-}" ]; then log "mcp: MEMORY_BIN=$MEMORY_BIN is not executable"; return 0; fi
	bg_install
	older=$(newest_installed)
	if [ -n "$older" ]; then
		log "mcp: no binary at $bin; using $older while it installs"
		exec "$older" mcp
	fi
	n=${MEMORY_MCP_WAIT:-25}
	while [ "$n" -gt 0 ] && [ -d "$lock" ] && [ ! -x "$bin" ]; do
		sleep 1
		n=$((n - 1))
	done
	[ -x "$bin" ] && exec "$bin" mcp
	log "mcp: no binary at $bin"
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

# spool_raw copies the hook payload into the daemon's compaction spool when the binary is
# missing or failed, so a compaction summary is not lost; the daemon redacts and validates it
# on ingest. The spool is <state dir>/compactions, the binary's own choice of state dir.
spool_raw() {
	if [ -n "${MEMORY_DIR-}" ]; then
		sdir=$MEMORY_DIR
	else
		case ${XDG_DATA_HOME-} in
		/*) sdir=$XDG_DATA_HOME/memory ;;
		*) sdir=$HOME/.local/share/memory ;;
		esac
	fi
	sdir=$sdir/compactions
	f=raw-$(date +%s)-$$.json
	( umask 077
		mkdir -p "$sdir" && cp "$in" "$sdir/.$f.tmp" && mv "$sdir/.$f.tmp" "$sdir/$f"
	) 2>/dev/null || log "compact-capture: could not spool the payload into $sdir"
}

main() {
	mode=${1-}
	if [ "$mode" = mcp ]; then
		[ -x "$bin" ] && exec "$bin" mcp
		mcp_without_binary
		return 0
	fi
	# `cost` is run by a person at a terminal: no hook payload on stdin (a terminal never sends
	# EOF) and no hook watchdog.
	if [ "$mode" = cost ]; then
		shift
		[ -x "$bin" ] && exec "$bin" cost "$@"
		echo "memory: not installed yet; start a Claude Code session first" >&2
		return 0
	fi
	tmpd=$(mktemp -d "${TMPDIR:-/tmp}/memory-hook.XXXXXX" 2>/dev/null) || { log "no temp dir"; cat >/dev/null; return 0; }
	trap 'rm -rf "$tmpd"' EXIT
	in=$tmpd/in outf=$tmpd/out
	cat >"$in" 2>/dev/null
	case $mode in
	install) install_bin ;;
	install-locked) # the detached child of bg_install, which took the lock
		install_bin && start_daemon
		rm -rf "$lock"
		;;
	session-start)
		if [ -x "$bin" ]; then
			start_daemon
			emit context
		else
			bg_install
		fi
		;;
	compact-capture) # PostCompact: no stdout, and the summary must not be lost
		if [ -x "$bin" ] && run_bounded "$@"; then :; else spool_raw; fi
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
