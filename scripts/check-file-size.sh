#!/bin/sh
# check-file-size.sh — enforce the ~600-line cap on non-test Go source files.
#
# Why: the codebase silently accumulated 11 files over the limit. This guard
# keeps the standard from regressing again, especially with multiple agents
# editing the same tree.
#
# Rules:
#   - Non-test .go files under internal/ and cmd/ are capped at 600 lines.
#   - _test.go files are capped at 900 lines. Tests carry table data and fixture
#     setup that does not decompose the way production code does, so the cap is
#     looser — but not absent: a 1500-line test file is a merge-conflict magnet
#     when several agents edit one package at once, which is the same problem
#     the 600-line rule exists to solve.
#   - internal/lsp/protocol/types.go is permanently exempt (LSP spec type
#     catalogue that mirrors the upstream protocol — not decomposable).
#   - Files listed in scripts/.filesize-baseline are "grandfathered": capped at
#     their recorded line count, not the rule, so they can't grow while they
#     await a split. Remove a file from the baseline once it drops under the cap.
set -eu

LIMIT=600
TEST_LIMIT=900
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BASELINE="$ROOT/scripts/.filesize-baseline"
PERMANENT="internal/lsp/protocol/types.go"

cd "$ROOT"

status=0
files=$(find internal cmd -name '*.go' | sort)
for rel in $files; do
	[ "$rel" = "$PERMANENT" ] && continue
	lines=$(wc -l <"$rel" | tr -d ' ')

	# The applicable rule depends on whether this is a test file.
	case "$rel" in
	*_test.go) limit=$TEST_LIMIT ;;
	*) limit=$LIMIT ;;
	esac

	cap=$limit
	if [ -f "$BASELINE" ]; then
		b=$(awk -v p="$rel" '$1 == p { print $2 }' "$BASELINE")
		[ -n "$b" ] && cap=$b
	fi

	if [ "$lines" -gt "$cap" ]; then
		echo "file-size: $rel has $lines lines (cap $cap)"
		status=1
	elif [ "$cap" -ne "$limit" ] && [ "$lines" -le "$limit" ]; then
		# Grandfathered file is now within the real limit — nudge to clean up.
		echo "file-size: note — $rel is now $lines lines; drop it from scripts/.filesize-baseline"
	fi
done

if [ "$status" -ne 0 ]; then
	echo ""
	echo "Split oversized files by responsibility (see the ~600-line rule in AGENTS.md/CLAUDE.md;"
	echo "test files get $TEST_LIMIT). Split tests by the behaviour under test, not arbitrarily."
	echo "Genuinely indivisible files can be added to scripts/.filesize-baseline with justification."
	exit 1
fi

echo "file-size: OK (all Go files within their cap: $LIMIT source, $TEST_LIMIT test)"
