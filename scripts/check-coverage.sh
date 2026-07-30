#!/bin/sh
# check-coverage.sh — measure statement coverage and enforce a floor.
#
# Why a floor and not a target: AGENTS.md requires "meaningful coverage" for
# internal/lsp, internal/cache and internal/tools, but nothing measured it, so
# the requirement could only ever be honoured by memory. A floor turns it into a
# gate. It is deliberately set just under the current figure — the point is to
# catch a regression, not to force a number.
#
# -coverpkg=./... instruments every package in the module, not just the ones a
# test binary happens to touch: a package with no tests enters the profile at
# 0%, so adding an untested package drags the total down and deleting a
# package's only test file can never raise it. Without it both holes were
# invisible to the floor.
#
# Ratchet policy: when the tree sits comfortably above the floor, raise FLOOR.
# Never lower it to make a red build green; add tests instead.
#
# Usage:
#   scripts/check-coverage.sh            # measure + enforce FLOOR
#   FLOOR=75 scripts/check-coverage.sh   # override the floor
#   scripts/check-coverage.sh --report   # also print the 20 least-covered packages
#
# Run on Linux for a comparable number: internal/fsguard is Darwin-only and its
# statements are unreachable (and correctly t.Skip'd) elsewhere, so the total
# differs by platform. CI enforces on ubuntu only for that reason.
set -eu

FLOOR="${FLOOR:-71.5}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PROFILE="${PROFILE:-$ROOT/.testcache/coverage.out}"

cd "$ROOT"
mkdir -p "$(dirname "$PROFILE")"

echo "coverage: running tests with -covermode=atomic (this takes a few minutes)…"
GOTMPDIR="$ROOT/.testcache" go test -covermode=atomic -coverpkg=./... -coverprofile="$PROFILE" ./... >/dev/null

total=$(go tool cover -func="$PROFILE" | awk '/^total:/ {gsub(/%/,"",$NF); print $NF}')
if [ -z "$total" ]; then
	echo "coverage: could not parse a total from $PROFILE" >&2
	exit 1
fi

if [ "${1:-}" = "--report" ]; then
	echo ""
	echo "coverage: 20 least-covered packages"
	go tool cover -func="$PROFILE" | awk -F'\t+' '
		$1 !~ /^total:/ {
			pkg = $1; sub(/\/[^/]+\.go:.*/, "", pkg)
			c = $NF; gsub(/%/, "", c)
			n[pkg]++; s[pkg] += c
		}
		END { for (p in n) printf "  %6.1f%%  %s\n", s[p]/n[p], p }
	' | sort -n | head -20
	echo ""
fi

# POSIX sh has no float compare; awk decides and sets the exit status.
if awk -v t="$total" -v f="$FLOOR" 'BEGIN { exit !(t + 0 < f + 0) }'; then
	echo "coverage: FAIL — total $total% is below the $FLOOR% floor"
	echo ""
	echo "Add tests for the code you changed. If the drop is legitimate and"
	echo "understood, adjust FLOOR in this script in the same commit, with a"
	echo "reason in the commit message."
	exit 1
fi

echo "coverage: OK — total $total% (floor $FLOOR%)"
