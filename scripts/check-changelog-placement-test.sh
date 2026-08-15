#!/bin/sh
# check-changelog-placement-test.sh — replay the real incidents through the guard.
#
# Why: check-changelog-placement.sh exists because of specific commits, so it is
# tested against those commits rather than against invented fixtures. Every case
# below is a real SHA in this repository's history, replayed with
# `--base <sha>^ --head <sha>`; the expectations are what the guard MUST say about
# changes that actually happened. Four of them shipped and had to be unpicked by
# hand; six are shapes a naive check would flag but which are perfectly legal.
#
# The false-positive half is the important half — a guard that fails a release
# commit or a changelog cleanup gets bypassed, and then it guards nothing. Note in
# particular that PR #325 (b7acf2e5) is a WEAK negative: its diff is 51 deletions and
# 0 additions, so it can only ever pass. The load-bearing negatives are the two
# `chore(release)` commits, which add a dated heading plus a trailing blank line
# below the top HTML comment, and the three relocation commits.
#
# A missing ref is an ERROR, not a skip: this runs in CI, which checks out with
# fetch-depth: 0, and a "0 of 10 cases ran" pass would be a green lie.
#
# Two of the guard's rules turn out to have NO real-history coverage — established by
# deleting each rule and finding all ten commits above still behaved. Those two get
# synthetic fixtures at the end of this file rather than going untested:
#
#   - blank added lines are ignored (a whitespace-only edit to an old section)
#   - an added line whose text was also deleted in the same diff is a relocation
#   - the section compared against is the BASE's first heading, not the new file's
#
# Each synthetic pair ships with a control case that MUST fail, so a broken harness
# cannot make them pass vacuously.
set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

GUARD="./scripts/check-changelog-placement.sh"

# want  sha       what it is
CASES="
fail f9451a42 appended a second '## 0.16.6' heading + 51 lines; PR #325 unpicked it
fail 53bb60a4 PR #320 — 44 lines under 0.16.6 while 0.16.7 was the unreleased section
fail 3fa005e7 PR #292 — 47 lines ~1250 lines down, under the third heading in the file
fail 3e885aca PR #310 — subject says 'move into 0.16.7'; it moved them into 0.16.6
pass b7acf2e5 PR #325 — the dedupe: 51 deletions, 0 additions
pass 1987390d PR #320's relocation into the unreleased section
pass abb85a9f PR #292's relocation, five hunks
pass 0e00b2bc PR #293's relocation, folded into a code commit
pass 6e15982a chore(release) 0.16.6 — dated heading plus a trailing blank line
pass 4e140bfe chore(release) 0.16.5 — same, and it added the top HTML comment
"

total=0
failed=0

echo "check-changelog-placement-test: replaying real commits through the guard"
echo ""

# Fed by a here-document, not a pipe, so the counters survive the loop.
while IFS= read -r line; do
	[ -n "$line" ] || continue
	want=${line%% *}
	rest=${line#* }
	sha=${rest%% *}
	what=${rest#* }
	total=$((total + 1))

	if ! git rev-parse --verify -q "$sha^{commit}" >/dev/null; then
		printf '  ERROR %s does not resolve — this needs full history (fetch-depth: 0)\n' "$sha"
		failed=$((failed + 1))
		continue
	fi

	if out=$("$GUARD" --base "$sha^" --head "$sha" 2>&1); then rc=0; else rc=$?; fi

	case "$want:$rc" in
	fail:1 | pass:0)
		printf '  ok    %s  %s — %s\n' "$sha" "$want" "$what"
		;;
	*)
		printf '  FAIL  %s  wanted %s, got exit %d — %s\n' "$sha" "$want" "$rc" "$what"
		printf '%s\n' "$out" | sed 's/^/          | /'
		failed=$((failed + 1))
		;;
	esac
done <<CASES_EOF
$CASES
CASES_EOF

# ── Synthetic cases, for the rules no real commit exercises ──────────────────────
#
# The guard resolves its own repo root from $0, so each fixture gets a throwaway
# repository with a copy of the guard in it.

SYNTH_BASE='# Changelog

## 0.2.0 (unreleased)

### Fixed

- an entry in the right place

## 0.1.0 (2026-01-01)

### Added

- first released entry
- second released entry'

# synth <pass|fail> <name>, with the post-change CHANGELOG.md on stdin.
synth() {
	want="$1"
	name="$2"
	total=$((total + 1))
	d="$(mktemp -d)"
	mkdir -p "$d/scripts"
	cp "$GUARD" "$d/scripts/check-changelog-placement.sh"
	git -C "$d" init -q >/dev/null 2>&1
	printf '%s\n' "$SYNTH_BASE" >"$d/CHANGELOG.md"
	git -C "$d" add -A
	git -C "$d" -c user.email=t@example.com -c user.name=Test commit -qm base
	synth_base_sha="$(git -C "$d" rev-parse HEAD)"
	cat >"$d/CHANGELOG.md"
	git -C "$d" add -A
	git -C "$d" -c user.email=t@example.com -c user.name=Test commit -qm head
	synth_head_sha="$(git -C "$d" rev-parse HEAD)"

	if out=$("$d/scripts/check-changelog-placement.sh" \
		--base "$synth_base_sha" --head "$synth_head_sha" 2>&1); then rc=0; else rc=$?; fi
	rm -rf "$d"

	case "$want:$rc" in
	fail:1 | pass:0)
		printf '  ok    %-38s %s\n' "$name" "$want"
		;;
	*)
		printf '  FAIL  %-38s wanted %s, got exit %d\n' "$name" "$want" "$rc"
		printf '%s\n' "$out" | sed 's/^/          | /'
		failed=$((failed + 1))
		;;
	esac
}

echo ""
echo "check-changelog-placement-test: synthetic cases for the uncovered rules"
echo ""

synth pass 'blank line in old section' <<'EOF'
# Changelog

## 0.2.0 (unreleased)

### Fixed

- an entry in the right place

## 0.1.0 (2026-01-01)

### Added

- first released entry

- second released entry
EOF

synth pass 'entry relocated into old section' <<'EOF'
# Changelog

## 0.2.0 (unreleased)

### Fixed

## 0.1.0 (2026-01-01)

### Added

- first released entry
- second released entry
- an entry in the right place
EOF

# A branch whose merge-base predates a release: something opened a newer unreleased
# section above the one this branch was written against, without re-stamping it (this
# repo has 50+ historical '(unreleased)' headings from exactly that era). The entry
# under 0.2.0 was correct when written and must stay legal — this is the case that
# fails if the guard ever compares against the NEW file's first heading.
synth pass 'entry under the base unreleased head' <<'EOF'
# Changelog

## 0.3.0 (unreleased)

### Added

- something released later

## 0.2.0 (unreleased)

### Fixed

- an entry in the right place
- a second entry in the right place

## 0.1.0 (2026-01-01)

### Added

- first released entry
- second released entry
EOF

# The control. If the harness were broken (guard skipping, base unresolved) the cases
# above would pass for the wrong reason; this one proves it really runs.
synth fail 'new entry in old section (control)' <<'EOF'
# Changelog

## 0.2.0 (unreleased)

### Fixed

- an entry in the right place

## 0.1.0 (2026-01-01)

### Added

- first released entry
- second released entry
- a genuinely new entry
EOF

echo ""
if [ "$failed" -ne 0 ]; then
	echo "check-changelog-placement-test: $failed of $total case(s) FAILED"
	exit 1
fi
echo "check-changelog-placement-test: OK ($total/$total cases behave as expected)"
