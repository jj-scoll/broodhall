#!/bin/sh
# guardrails hook launcher: runs this plugin version's claude-tools binary on the hook's stdin.
# `run.sh install` (the SessionStart hook) downloads the binary from the GitHub release the
# first time a version runs, records its sha256 next to it, then runs `claude-tools doctor
# -hook`, which says nothing unless the install has a problem. A failed install or a failed
# doctor never stops the session start. The guards
# (pre-tool-use, read-dedup, large-read-guard, config-gate) fail CLOSED: with no binary they exit
# 2 and say on stderr what failed, how to retry and how to opt out, so PreToolUse blocks the
# tool call instead of letting it through unguarded. Every other hook exits 0 silently, since
# exit 2 at Stop would keep Claude from ending its turn and at PreCompact would block compaction.
# The opt-out is the fail_open option (CLAUDE_PLUGIN_OPTION_FAIL_OPEN) or, outside the plugin,
# CLAUDE_TOOLS_FAIL_OPEN: either one set to true, 1 or yes makes a missing binary exit 0 silently.
# shellcheck disable=SC2015 # `a && b || fail`: fail when any step fails, which is intended
set -u
root=${CLAUDE_PLUGIN_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}
data=${CLAUDE_PLUGIN_DATA:-$HOME/.claude/plugins/data/guardrails}
ver=$(sed -n 's/^ *"version": *"\([^"]*\)".*/\1/p' "$root/.claude-plugin/plugin.json")
dest=$data/bin/$ver/claude-tools
bin=${CLAUDE_TOOLS_BIN:-$dest}
export CLAUDE_PLUGIN_ROOT="$root"

# userConfig options reach hooks as CLAUDE_PLUGIN_OPTION_<KEY>. An option the user never set
# falls back to the same env var outside the plugin, then to plugin.json's default.
export CLAUDE_TOOLS_OFF="${CLAUDE_PLUGIN_OPTION_DISABLED_GUARDS-${CLAUDE_TOOLS_OFF-}}"
export CLAUDE_TOOLS_PII_ALLOW="${CLAUDE_PLUGIN_OPTION_ALLOWED_EMAIL_DOMAINS-${CLAUDE_TOOLS_PII_ALLOW-}}"
# Client-data rules come from the user; with none set they do nothing (see docs/guardrails/pii.md).
export CLAUDE_TOOLS_CLIENT_BUCKETS="${CLAUDE_PLUGIN_OPTION_CLIENT_DATA_BUCKETS-${CLAUDE_TOOLS_CLIENT_BUCKETS-}}"
export CLAUDE_TOOLS_MASKED_DIRS="${CLAUDE_PLUGIN_OPTION_MASKED_COPY_DIRS-${CLAUDE_TOOLS_MASKED_DIRS-}}"
export HANDOFF_TOKENS="${CLAUDE_PLUGIN_OPTION_HANDOFF_TOKENS-${HANDOFF_TOKENS-}}"
# The opt-in rtk rewrite is off unless the rtk option (or CLAUDE_TOOLS_RTK) says on.
export CLAUDE_TOOLS_RTK="${CLAUDE_PLUGIN_OPTION_RTK-${CLAUDE_TOOLS_RTK-}}"
export CLAUDE_TOOLS_RTK_CMD="${CLAUDE_PLUGIN_OPTION_RTK_COMMAND-${CLAUDE_TOOLS_RTK_CMD-}}"
# Per-session guard state stays with this user's plugin data, not in a shared /tmp dir.
export HANDOFF_STATE_DIR="${HANDOFF_STATE_DIR-$data}"

case ${CLAUDE_PLUGIN_OPTION_FAIL_OPEN-${CLAUDE_TOOLS_FAIL_OPEN-}} in
true | TRUE | True | 1 | yes) fail_open=1 ;;
*) fail_open= ;;
esac

if [ "${1-}" = install ]; then
	doctor() { "$bin" doctor -hook || true; exit 0; }
	[ -x "$bin" ] && doctor
	fail() {
		if [ -n "$fail_open" ]; then
			effect="so its guards are off this session (fail_open is set)"
		else
			effect="so tool calls are blocked until it installs (set the fail_open option to allow them unguarded)"
		fi
		printf '{"systemMessage":"guardrails: could not install claude-tools %s (%s), %s. It retries at the next session start."}\n' "$ver" "$1" "$effect"
		exit 0
	}
	base=${CLAUDE_TOOLS_RELEASE_URL:-https://github.com/jj-scoll/broodhall/releases/download}
	os=$(uname -s | tr '[:upper:]' '[:lower:]')
	case $(uname -m) in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) fail "unsupported CPU $(uname -m)" ;;
	esac
	asset=claude-tools_${os}_$arch.tar.gz
	mkdir -p "$data/bin" && tmp=$(mktemp -d "$data/bin/.download.XXXXXX") || fail "cannot write $data"
	trap 'rm -rf "$tmp"' EXIT
	curl -fsSL --max-time 60 -o "$tmp/$asset" "$base/v$ver/$asset" &&
		curl -fsSL --max-time 30 -o "$tmp/checksums.txt" "$base/v$ver/checksums.txt" ||
		fail "download of $asset failed"
	if command -v sha256sum >/dev/null; then sum=sha256sum; else sum='shasum -a 256'; fi
	# Pick the asset's line first: `$sum -c` on empty input exits 0 or errors depending on the
	# shasum build, so an asset missing from checksums.txt is refused here, by name.
	line=$(grep "  $asset\$" "$tmp/checksums.txt")
	[ -n "$line" ] || fail "no checksum for $asset in checksums.txt"
	(cd "$tmp" && printf '%s\n' "$line" | $sum -c - >/dev/null 2>&1) || fail "checksum mismatch"
	tar -xzf "$tmp/$asset" -C "$tmp" claude-tools && mkdir -p "${dest%/*}" &&
		mv -f "$tmp/claude-tools" "$dest" || fail "unpack failed"
	# What doctor compares the binary with later: the unpacked file's hash, taken after the
	# archive passed the release checksum.
	$sum "$dest" | sed 's|  .*|  claude-tools|' >"$dest.sha256"
	doctor
fi

if [ ! -x "$bin" ]; then
	cat >/dev/null
	case ${1-} in
	pre-tool-use | read-dedup | large-read-guard | config-gate) ;;
	*) exit 0 ;;
	esac
	[ -n "$fail_open" ] && exit 0
	{
		printf 'guardrails: BLOCKED: the guard binary claude-tools %s is not installed (%s), so this tool call was not checked.\n' "$ver" "$bin"
		printf 'Retry: start a new session (SessionStart downloads it), or run: sh "%s/scripts/run.sh" install\n' "$root"
		printf 'Opt out: set the guardrails option fail_open to true (or CLAUDE_TOOLS_FAIL_OPEN=1) to allow tool calls without guards.\n'
	} >&2
	exit 2
fi
exec "$bin" "$@"
