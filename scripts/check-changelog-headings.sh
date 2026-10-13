#!/bin/sh
# check-changelog-headings.sh — catch a duplicate CHANGELOG version heading.
#
# Why: a rebase can silently replay a CHANGELOG.md addition as a bare new
# '## <version> (date)' heading rather than a conflicting insertion under the
# existing one — git sees a pure addition, so it never conflicts. This bit
# main at least three times by hand (PR #320, #292, #293 each needed a
# by-hand relocation) before an actual duplicate `## 0.16.6 (...)` heading
# landed uncaught (PLAN-313). This guard fails when any version number
# appears in more than one heading.
set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CHANGELOG="$ROOT/CHANGELOG.md"

dupes=$(grep -E '^## [0-9]+\.[0-9]+\.[0-9]+' "$CHANGELOG" \
	| awk '{print $2}' \
	| sort \
	| uniq -d)

if [ -n "$dupes" ]; then
	echo "check-changelog: duplicate version heading(s) in CHANGELOG.md:"
	echo "$dupes" | while read -r v; do
		grep -Fn "## $v " "$CHANGELOG" | sed 's/^/  line /'
	done
	echo ""
	echo "Each version must have exactly one '## <version>' heading. Merge the"
	echo "sections by hand — do not just delete one, entries under both may be"
	echo "real (see plumb-ops board card PLAN-313 for a worked example)."
	exit 1
fi

# Within-version shapes. A rebase that replays a whole subsection as additions
# leaves every line under the right '## ' heading, so the checks above and the
# placement guard pass it. Released versions carry legacy repeats (0.17.x,
# 0.16.x), so only the topmost version — the one new work lands in — is held to
# one of each '### ' subsection and no verbatim-repeated '- ' entry.
inner=$(awk '
	/^## [0-9]+\.[0-9]+\.[0-9]+/ { if (seen) exit; seen = 1; ver = $2; next }
	!seen { next }
	/^### / { if ($0 in sub_) print "  line " NR ": duplicate subsection \"" $0 "\" (first at line " sub_[$0] ")"; else sub_[$0] = NR; next }
	/^- /   { if ($0 in ent) print "  line " NR ": duplicate entry \"" substr($0, 1, 70) "...\" (first at line " ent[$0] ")"; else ent[$0] = NR }
' "$CHANGELOG")

if [ -n "$inner" ]; then
	echo "check-changelog: the topmost version in CHANGELOG.md repeats a subsection or an entry:"
	echo "$inner"
	echo ""
	echo "A rebase most likely replayed a section as additions. Keep one '### ' of"
	echo "each kind per version and one copy of each entry."
	exit 1
fi

echo "check-changelog: OK (no duplicate version headings, subsections or entries)"
