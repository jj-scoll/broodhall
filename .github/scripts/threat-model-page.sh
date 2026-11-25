#!/bin/sh
# threat-model-page.sh <THREAT_MODEL.md> <out.md>
# Writes the docs-site copy of the threat model. THREAT_MODEL.md stays at the repository root,
# where GitHub shows it and TestThreatModelDoc reads it, and its guard links are repo-relative
# (docs/guardrails/<name>.md). The copy is served from docs/reference/, so those links become
# ../guardrails/<name>.md. pages.yml runs this before the site build; the output is gitignored.
set -eu
[ "$#" -eq 2 ] || { echo "usage: threat-model-page.sh <THREAT_MODEL.md> <out.md>" >&2; exit 2; }
[ -f "$1" ] || { echo "threat-model-page.sh: $1: no such file" >&2; exit 1; }
{
	echo '<!-- Generated from THREAT_MODEL.md by .github/scripts/threat-model-page.sh. Edit THREAT_MODEL.md, not this file. -->'
	sed 's#](docs/#](../#g' "$1"
} >"$2"
