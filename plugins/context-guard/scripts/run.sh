#!/bin/sh
# context-guard launcher: the mod calls it to reach the claude-tools binary that the guardrails
# plugin installed, and to learn where guardrails keeps its per-session handoff state.
#   run.sh state              prints the state base dir ($base/claude-handoff holds the marks)
#   run.sh autoclear <args>   runs `claude-tools autoclear <args>` with that dir
# With no binary or no state dir it prints nothing and exits 1, so the mod skips the clear.
# Both lookups mirror guardrails' launcher: CLAUDE_TOOLS_BIN names a binary, HANDOFF_STATE_DIR
# names the state dir (an empty value means /tmp, as in the binary), and otherwise the state
# dir is guardrails' plugin data dir, where its launcher unpacks bin/<version>/claude-tools.
set -u
cfg=${CLAUDE_CONFIG_DIR:-$HOME/.claude}

# The most recently installed guardrails binary.
newest=
for f in "$cfg"/plugins/data/guardrails-*/bin/*/claude-tools; do
	[ -x "$f" ] || continue
	if [ -z "$newest" ] || [ "$f" -nt "$newest" ]; then newest=$f; fi
done
base=
if [ "${HANDOFF_STATE_DIR+set}" ]; then
	base=${HANDOFF_STATE_DIR:-/tmp}
elif [ -n "$newest" ]; then
	base=${newest%/bin/*/claude-tools}
fi
bin=${CLAUDE_TOOLS_BIN-$newest}

case "${1-}" in
state)
	[ -n "$base" ] || exit 1
	printf '%s\n' "$base"
	;;
autoclear)
	[ -n "$base" ] && [ -x "$bin" ] || exit 1
	HANDOFF_STATE_DIR=$base
	export HANDOFF_STATE_DIR
	exec "$bin" "$@"
	;;
*) exit 2 ;;
esac
