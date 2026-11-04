#!/bin/sh
# guardrails hook launcher: runs this plugin version's claude-tools binary on the hook's stdin.
# `run.sh install` (the SessionStart hook) downloads the binary from the GitHub release the
# first time a version runs. Every other call fails open: with no binary it exits 0 and
# prints nothing, which allows the tool call, so a failed download never stops a session.
# shellcheck disable=SC2015 # `a && b || fail`: fail when any step fails, which is intended
set -u
root=${CLAUDE_PLUGIN_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}
data=${CLAUDE_PLUGIN_DATA:-$HOME/.claude/plugins/data/guardrails}
ver=$(sed -n 's/^ *"version": *"\([^"]*\)".*/\1/p' "$root/.claude-plugin/plugin.json")
dest=$data/bin/$ver/claude-tools
bin=${CLAUDE_TOOLS_BIN:-$dest}

# userConfig options reach hooks as CLAUDE_PLUGIN_OPTION_<KEY>. An option the user never set
# falls back to the same env var outside the plugin, then to plugin.json's default.
export CLAUDE_TOOLS_OFF="${CLAUDE_PLUGIN_OPTION_DISABLED_GUARDS-${CLAUDE_TOOLS_OFF-pii,raw-client-data,session-url}}"
export CLAUDE_TOOLS_PII_ALLOW="${CLAUDE_PLUGIN_OPTION_ALLOWED_EMAIL_DOMAINS-${CLAUDE_TOOLS_PII_ALLOW-}}"
export HANDOFF_TOKENS="${CLAUDE_PLUGIN_OPTION_HANDOFF_TOKENS-${HANDOFF_TOKENS-}}"
# Per-session guard state stays with this user's plugin data, not in a shared /tmp dir.
export HANDOFF_STATE_DIR="${HANDOFF_STATE_DIR-$data}"

if [ "${1-}" = install ]; then
	[ -x "$bin" ] && exit 0
	fail() {
		printf '{"systemMessage":"guardrails: could not install claude-tools %s (%s), so its guards are off this session. It retries at the next session start."}\n' "$ver" "$1"
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
	(cd "$tmp" && grep "  $asset\$" checksums.txt | $sum -c - >/dev/null 2>&1) || fail "checksum mismatch"
	tar -xzf "$tmp/$asset" -C "$tmp" claude-tools && mkdir -p "${dest%/*}" &&
		mv -f "$tmp/claude-tools" "$dest" || fail "unpack failed"
	exit 0
fi

if [ ! -x "$bin" ]; then
	cat >/dev/null
	exit 0
fi
exec "$bin" "$@"
