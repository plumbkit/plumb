#!/bin/sh
# check-agents-brief.sh — keep AGENTS.md a brief, not a book.
#
# Why: the brief grew to ~527 lines / 91 KB, an onboarding tax paid by every
# agent session that loads it. Rules and pointers belong in AGENTS.md;
# reference detail belongs in docs/. This guard fails when the file grows
# past the budget, forcing detail back into the docs pages the brief links to.
#
# Budget: 150 lines AND 20 KiB. Both caps are deliberate: the line cap keeps the
# file scannable, while the byte cap leaves room for an outer workspace brief in
# clients that concatenate nested project instructions. Raise either only in the
# same commit that explains why the always-loaded context needs the headroom.
set -eu

MAX_LINES=150
MAX_BYTES=20480
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BRIEF="$ROOT/AGENTS.md"

lines=$(wc -l <"$BRIEF" | tr -d ' ')
bytes=$(wc -c <"$BRIEF" | tr -d ' ')

status=0
if [ "$lines" -gt "$MAX_LINES" ]; then
	echo "agents-brief: AGENTS.md has $lines lines (budget $MAX_LINES)"
	status=1
fi
if [ "$bytes" -gt "$MAX_BYTES" ]; then
	echo "agents-brief: AGENTS.md is $bytes bytes (budget $MAX_BYTES)"
	status=1
fi

if [ "$status" -ne 0 ]; then
	echo ""
	echo "AGENTS.md is rules + pointers, not reference documentation. Move the"
	echo "detail into the docs page the brief links to (docs/configuration.md,"
	echo "docs/architecture.md, docs/cli-reference.md, docs/adding-an-lsp.md,"
	echo "docs/contributing.md) and keep the pointer line."
	exit 1
fi

echo "agents-brief: OK (AGENTS.md is $lines lines / $bytes bytes; budget $MAX_LINES lines / $MAX_BYTES bytes)"
