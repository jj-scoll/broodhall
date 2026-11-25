#!/bin/bash
# Tests for .github/scripts/threat-model-page.sh <THREAT_MODEL.md> <out.md>: the docs-site copy
# of the threat model, with its repo-root links to docs/ rewritten for a page in docs/reference/.
# shellcheck disable=SC2015,SC2013 # ok and bad always succeed; page names have no spaces
here=$(cd "$(dirname "$0")/.." && pwd)
repo=$(cd "$here/../.." && pwd)
tm=$here/threat-model-page.sh
pass=0 fail=0
ok() { pass=$((pass + 1)); echo "ok   $1"; }
bad() { fail=$((fail + 1)); echo "FAIL $1: $2"; }

t=$(mktemp -d)
trap 'rm -rf "$t"' EXIT

# --- 1. links
cat >"$t/in.md" <<'EOF'
# Threat model: guardrails

See [Git](docs/guardrails/git.md) and [Bash core](docs/guardrails/bash-core.md#rules), then
[the rows](#threats) and [OWASP](https://genai.owasp.org/llm-top-10/).
EOF
if "$tm" "$t/in.md" "$t/out.md"; then ok "exits 0"; else bad "exits 0" "exited $?"; fi
grep -qF '[Git](../guardrails/git.md)' "$t/out.md" && ok "docs/ link rewritten" || bad "docs/ link rewritten" "$(cat "$t/out.md")"
grep -qF '[Bash core](../guardrails/bash-core.md#rules)' "$t/out.md" && ok "docs/ link with anchor rewritten" || bad "docs/ link with anchor rewritten" "$(cat "$t/out.md")"
grep -qF '[the rows](#threats)' "$t/out.md" && ok "in-page anchor kept" || bad "in-page anchor kept" "$(cat "$t/out.md")"
grep -qF '[OWASP](https://genai.owasp.org/llm-top-10/)' "$t/out.md" && ok "absolute link kept" || bad "absolute link kept" "$(cat "$t/out.md")"
grep -q '](docs/' "$t/out.md" && bad "no docs/ link left" "$(cat "$t/out.md")" || ok "no docs/ link left"
[ "$(head -n 1 "$t/out.md")" = '<!-- Generated from THREAT_MODEL.md by .github/scripts/threat-model-page.sh. Edit THREAT_MODEL.md, not this file. -->' ] &&
	ok "generated-file note first" || bad "generated-file note first" "$(head -n 1 "$t/out.md")"
[ "$(sed -n 2p "$t/out.md")" = '# Threat model: guardrails' ] && ok "title kept" || bad "title kept" "$(sed -n 2p "$t/out.md")"

# --- 2. the real THREAT_MODEL.md: every rewritten link lands on a page in docs/
"$tm" "$repo/THREAT_MODEL.md" "$t/real.md" || bad "real THREAT_MODEL.md" "exited $?"
n=0 missing=''
for p in $(grep -o '](\.\./[^)#]*' "$t/real.md" | sed 's#^](\.\./##' | sort -u); do
	n=$((n + 1))
	[ -f "$repo/docs/$p" ] || missing="$missing $p"
done
[ "$n" -gt 0 ] && [ -z "$missing" ] && ok "real links resolve ($n pages)" || bad "real links resolve" "n=$n missing:$missing"

# --- 3. usage
if "$tm" "$t/in.md" >/dev/null 2>&1; then bad "one argument fails" "exited 0"; else ok "one argument fails"; fi
if "$tm" "$t/nonexistent.md" "$t/x.md" >/dev/null 2>&1; then bad "missing input fails" "exited 0"; else ok "missing input fails"; fi
[ -e "$t/x.md" ] && bad "missing input writes nothing" "$t/x.md exists" || ok "missing input writes nothing"

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
