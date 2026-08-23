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
#
# A third block covers the shape neither of the first two can express: a base branch
# that MOVES ON after the fork. Those cases need three commits, and they are where R4
# lives — an entry written into the open section, which the base then released. Two
# of them are expectations that used to read the other way round; see the block.
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

# A base whose first column-0 '## ' line is quoted inside a fence, above the real
# unreleased heading. Only a fence-aware scan finds 0.2.0 rather than 9.9.9.
SYNTH_BASE_FENCED_HEAD='# Changelog

```
## 9.9.9 (not a heading)
```

## 0.2.0 (unreleased)

### Fixed

- an entry in the right place

## 0.1.0 (2026-01-01)

### Added

- first released entry'

# A base whose UNRELEASED section carries a '### Added' sub-heading, so a case can
# delete it (a reverted feature) and re-add the identical line elsewhere.
SYNTH_BASE_REVERT='# Changelog

## 0.3.0 (unreleased)

### Added

- a feature that got reverted

## 0.2.0 (2026-02-01)

### Fixed

- an entry that shipped in 0.2.0

## 0.1.0 (2026-01-01)

### Fixed

- first released entry'

# A base with TWO already-released sections, for cases about moving between them.
SYNTH_BASE_TWO_RELEASES='# Changelog

## 0.3.0 (unreleased)

### Fixed

- an unreleased entry

## 0.2.0 (2026-02-01)

### Added

- an entry that shipped in 0.2.0

## 0.1.0 (2026-01-01)

### Added

- first released entry'

SYNTH_BASE='# Changelog

## 0.2.0 (unreleased)

### Fixed

- an entry in the right place

## 0.1.0 (2026-01-01)

### Added

- first released entry
- second released entry'

# synth <pass|fail> <name> [base-content], with the post-change CHANGELOG.md on stdin.
# base-content defaults to $SYNTH_BASE; pass one explicitly when a case needs a shape
# the shared base cannot express (e.g. two already-released sections).
# Set SYNTH_ALLOW=1 around a call to run it with CHANGELOG_PLACEMENT_ALLOW set.
synth() {
	want="$1"
	name="$2"
	base_md="${3:-$SYNTH_BASE}"
	total=$((total + 1))
	d="$(mktemp -d)"
	mkdir -p "$d/scripts"
	cp "$GUARD" "$d/scripts/check-changelog-placement.sh"
	git -C "$d" init -q >/dev/null 2>&1
	printf '%s\n' "$base_md" >"$d/CHANGELOG.md"
	git -C "$d" add -A
	git -C "$d" -c user.email=t@example.com -c user.name=Test commit -qm base
	synth_base_sha="$(git -C "$d" rev-parse HEAD)"
	cat >"$d/CHANGELOG.md"
	git -C "$d" add -A
	git -C "$d" -c user.email=t@example.com -c user.name=Test commit -qm head
	synth_head_sha="$(git -C "$d" rev-parse HEAD)"

	if out=$(CHANGELOG_PLACEMENT_ALLOW="${SYNTH_ALLOW:-}" "$d/scripts/check-changelog-placement.sh" \
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

# R3. An entry MOVED OUT of the unreleased section and into a released one: the
# 3e885aca (#310) shape, where a relocation aimed at the unreleased heading landed on
# a released one. #310 only failed the guard because one incidental new line rode
# along with it; as a clean move it used to pass with an advisory note nobody would
# read, since nobody reads the log of a passing check.
synth fail 'entry moved OUT of unreleased (#310)' <<'EOF'
# Changelog

## 0.2.0 (unreleased)

### Fixed

## 0.1.0 (2026-01-01)

### Added

- first released entry
- second released entry
- an entry in the right place
EOF

# The other direction stays advisory. Moving an entry BETWEEN two already-released
# sections is a cleanup — correcting which shipped version an entry belongs to — and
# only a human can judge it, so R3 must not reach for it. This is the case that fails
# if R3 is ever widened from "out of unreleased" to "into anything released".
synth pass 'entry moved between released sections' "$SYNTH_BASE_TWO_RELEASES" <<'EOF'
# Changelog

## 0.3.0 (unreleased)

### Fixed

- an unreleased entry

## 0.2.0 (2026-02-01)

### Added

## 0.1.0 (2026-01-01)

### Added

- first released entry
- an entry that shipped in 0.2.0
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

# Rewriting an existing released entry is a hunk with deletions, so it is not an
# insertion and cannot be a replayed entry. Failing it would be a pure false positive
# on the commonest legitimate reason to touch an old section, and the guard's own
# header promises it stays free.
synth pass 'typo fixed in a released entry' <<'EOF'
# Changelog

## 0.2.0 (unreleased)

### Fixed

- an entry in the right place

## 0.1.0 (2026-01-01)

### Added

- first released entry, with the typo corrected
- second released entry
EOF

# The same rule from the other side: one released line reflowed into two. The hunk is
# -1/+2, so both added lines are exempt.
synth pass 'released entry reflowed across two lines' <<'EOF'
# Changelog

## 0.2.0 (unreleased)

### Fixed

- an entry in the right place

## 0.1.0 (2026-01-01)

### Added

- first released entry, now long enough that it wraps
  onto a second line
- second released entry
EOF

# An entry that quotes a CHANGELOG heading inside a fenced block — the guard's own
# entry does exactly this kind of thing. The fenced line is not a heading, so R2 must
# not read it as one added below the top of the file.
synth pass 'heading quoted inside a fenced block' <<'EOF'
# Changelog

## 0.2.0 (unreleased)

### Fixed

- an entry in the right place
- a guard that fails when an entry lands under a stamped heading:

```
## 0.1.0 (2026-01-01)
```

## 0.1.0 (2026-01-01)

### Added

- first released entry
- second released entry
EOF

# A release commit: it stamps the unreleased heading with a date and opens the next
# empty '(unreleased)' section above it (that is exactly what 6e15982a did). Both
# headings are lines this change added, so R1's added-heading clause covers them and
# no rule may reach for the stamped section. The load-bearing happy path for anything
# that keys on a date stamp, R4 included.
synth pass 'release stamps and opens the next' <<'EOF'
# Changelog

## 0.3.0 (unreleased)

## 0.2.0 (2026-02-01)

### Fixed

- an entry in the right place

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

# Independent review, probe C. fromunrel is keyed by TEXT, and '### Added' appears in
# nearly every section, so dropping a reverted feature from the unreleased section
# (taking its '### Added' with it) used to mark every later re-addition of that exact
# line as a move OUT of unreleased — hard-failing a legitimate cleanup with a
# confidently wrong diagnosis, in the same report where the actual entry move was
# correctly advisory. Sub-headings are exempt from R3 for this reason.
synth pass 'cleanup that re-adds a sub-heading' "$SYNTH_BASE_REVERT" <<'EOF'
# Changelog

## 0.3.0 (unreleased)

## 0.2.0 (2026-02-01)

### Fixed

## 0.1.0 (2026-01-01)

### Fixed

- first released entry

### Added

- an entry that shipped in 0.2.0
EOF

# Independent review, probe B. R3 is checked BEFORE the pure-addition gate, because a
# move is identified by the delete/re-add pair rather than by hunk shape. Editing the
# line above the landing site pairs the moved line into a rewrite hunk, which used to
# hide the #310 shape completely — no failure and no advisory note.
synth fail 'move out of unreleased, beside an edit' <<'EOF'
# Changelog

## 0.2.0 (unreleased)

### Fixed

## 0.1.0 (2026-01-01)

### Added

- first released entry, typo fixed
- an entry in the right place
- second released entry
EOF

# Independent review, finding 5. The base scan is fence-aware; without that, a heading
# quoted in an example ABOVE the real one is taken for the unreleased section and
# every comparison in the run comes out against the wrong version. This case is what
# pins it — reverting the base scan to a plain /^## / match leaves the whole suite
# green otherwise.
synth pass 'fenced heading above the real one' "$SYNTH_BASE_FENCED_HEAD" <<'EOF'
# Changelog

```
## 9.9.9 (not a heading)
```

## 0.2.0 (unreleased)

### Fixed

- an entry in the right place
- a second entry in the right place

## 0.1.0 (2026-01-01)

### Added

- first released entry
EOF

# The documented escape valve, which had no coverage at all: the control case above is
# a real violation, so if setting the variable does not turn it green the bypass is
# broken — and a bypass nobody can rely on is one people route around by deleting the
# step instead.
SYNTH_ALLOW=1
synth pass 'CHANGELOG_PLACEMENT_ALLOW bypasses a violation' <<'EOF'
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
SYNTH_ALLOW=""

# ── The CI shape: the base branch moves on after the fork ────────────────────────
#
# Everything above replays a linear base→head pair, where the base ref and the fork
# point are the same commit. A real PR is not that shape: main keeps moving after a
# branch is cut, and the guard is handed the base branch TIP plus the branch head.
#
# actions/checkout builds refs/pull/N/merge, whose parent is the base SHA, so a
# merge-base against the checked-out HEAD resolves to the base branch tip rather than
# the branch's fork point; passing the PR's head SHA restores the fork point, which is
# why the workflow does. What the fork point cannot say on its own is whether the
# section the branch is writing into has SHIPPED on the base since it was cut — that
# is R4's subject, and the reason these cases need three commits rather than two.
#
# The merge commit CI checks out is deliberately not built here: the guard is pointed
# at the base and head SHAs, never at the merge, so it would be scenery.

# forkcase <pass|fail> <name> <fork-md> <base-tip-md> <branch-md>
forkcase() {
	want="$1"
	name="$2"
	fork_md="$3"
	base_md="$4"
	branch_md="$5"
	total=$((total + 1))
	d="$(mktemp -d)"
	mkdir -p "$d/scripts"
	cp "$GUARD" "$d/scripts/check-changelog-placement.sh"
	git -C "$d" init -q >/dev/null 2>&1
	git -C "$d" symbolic-ref HEAD refs/heads/main
	printf '%s\n' "$fork_md" >"$d/CHANGELOG.md"
	git -C "$d" add CHANGELOG.md
	git -C "$d" -c user.email=t@example.com -c user.name=Test commit -qm fork

	# The branch. code.txt rides along so that a case whose CHANGELOG.md is untouched
	# still produces a real commit — "a PR that never touches the changelog" is one of
	# the cases, and it must reach the guard rather than fail to commit.
	git -C "$d" checkout -q -b feature
	printf '%s\n' "$branch_md" >"$d/CHANGELOG.md"
	printf 'branch\n' >"$d/code.txt"
	git -C "$d" add CHANGELOG.md code.txt
	git -C "$d" -c user.email=t@example.com -c user.name=Test commit -qm 'branch work'
	fork_head="$(git -C "$d" rev-parse HEAD)"

	# The base branch moves on afterwards — a release, or just another PR landing.
	git -C "$d" checkout -q main
	printf '%s\n' "$base_md" >"$d/CHANGELOG.md"
	printf 'main\n' >"$d/other.txt"
	git -C "$d" add CHANGELOG.md other.txt
	git -C "$d" -c user.email=t@example.com -c user.name=Test commit -qm 'base moves on'
	fork_base="$(git -C "$d" rev-parse HEAD)"

	if out=$("$d/scripts/check-changelog-placement.sh" \
		--base "$fork_base" --head "$fork_head" 2>&1); then rc=0; else rc=$?; fi
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

# The base tip after a release: 0.2.0 stamped, 0.3.0 opened above it, and an entry
# from a PR that landed before the cut. That last line matters — the branch's copy of
# the 0.2.0 section is NOT byte-identical to the base's, which is why "hash the
# released sections on both sides and compare" cannot be the check: every branch that
# is merely behind main would fail it.
FORK_BASE_RELEASED='# Changelog

## 0.3.0 (unreleased)

## 0.2.0 (2026-02-01)

### Fixed

- an entry in the right place
- an entry from a PR that landed before the release

## 0.1.0 (2026-01-01)

### Added

- first released entry
- second released entry'

# The base tip opened a newer section but stamped nothing. 0.2.0 is still open, so an
# entry under it is still correct — this repo carries 50+ historical '(unreleased)'
# headings from an era when releases were not dated, and R4 must key on the stamp.
FORK_BASE_UNSTAMPED='# Changelog

## 0.3.0 (unreleased)

## 0.2.0 (unreleased)

### Fixed

- an entry in the right place

## 0.1.0 (2026-01-01)

### Added

- first released entry
- second released entry'

# The base tip merely gained another entry in the same still-unreleased section.
FORK_BASE_AHEAD='# Changelog

## 0.2.0 (unreleased)

### Fixed

- an entry in the right place
- an entry from another PR, still unreleased

## 0.1.0 (2026-01-01)

### Added

- first released entry
- second released entry'

# The base tip stamped the top heading and opened nothing above it, so the file has no
# unreleased section at all. Not what chore(release) does here (6e15982a opens the next
# one), but the state a stamp-only release would leave, and an entry added into it is
# still an entry added into shipped history.
FORK_BASE_STAMPED_TOP='# Changelog

## 0.2.0 (2026-02-01)

### Fixed

- an entry in the right place

## 0.1.0 (2026-01-01)

### Added

- first released entry
- second released entry'

# A fork with two entries in the open section, so a case can reorder them: a move
# needs two lines to move between.
FORK_TWO_ENTRIES='# Changelog

## 0.2.0 (unreleased)

### Fixed

- alpha entry
- beta entry

## 0.1.0 (2026-01-01)

### Added

- first released entry'

FORK_TWO_RELEASED='# Changelog

## 0.3.0 (unreleased)

## 0.2.0 (2026-02-01)

### Fixed

- alpha entry
- beta entry

## 0.1.0 (2026-01-01)

### Added

- first released entry'

FORK_TWO_REORDERED='# Changelog

## 0.2.0 (unreleased)

### Fixed

- beta entry
- alpha entry

## 0.1.0 (2026-01-01)

### Added

- first released entry'

# A base tip whose still-open section QUOTES a stamped heading inside a fenced block —
# which is what an entry about this very guard looks like. 0.2.0 has not shipped there;
# only a fence-aware scan of the tip says so.
FORK_BASE_FENCED='# Changelog

## 0.3.0 (unreleased)

### Fixed

- a guard that fails when an entry lands under a stamped heading:

```
## 0.2.0 (2026-02-01)
```

## 0.2.0 (unreleased)

### Fixed

- an entry in the right place

## 0.1.0 (2026-01-01)

### Added

- first released entry
- second released entry'

# The branch rewrites a line that was already in the section, rather than adding one.
FORK_BRANCH_TYPO='# Changelog

## 0.2.0 (unreleased)

### Fixed

- an entry in the right place, typo fixed

## 0.1.0 (2026-01-01)

### Added

- first released entry
- second released entry'

# The branch: one entry appended to the section that was unreleased when it forked.
FORK_BRANCH_ENTRY='# Changelog

## 0.2.0 (unreleased)

### Fixed

- an entry in the right place
- a second entry, added on the branch

## 0.1.0 (2026-01-01)

### Added

- first released entry
- second released entry'

# The branch opened its own unreleased section above the one it forked from, which is
# what a branch caught by the case below is told to do.
FORK_BRANCH_NEW_SECTION='# Changelog

## 0.3.0 (unreleased)

### Fixed

- a second entry, added on the branch

## 0.2.0 (unreleased)

### Fixed

- an entry in the right place

## 0.1.0 (2026-01-01)

### Added

- first released entry
- second released entry'

echo ""
echo "check-changelog-placement-test: the base branch moving on under a branch"
echo ""

# THE DEFECT (PLAN-399, and PR #404 on 2026-08-22). The branch forked while 0.2.0 was
# the unreleased section and correctly wrote its entry there; 0.2.0 then SHIPPED on the
# base. Nothing about the branch changed, and a rebase replays the entry into the dated
# section without conflicting — so merging it puts a line into released history that
# never shipped with that release. Matching the fork point's heading by version number
# cannot see this: the branch's own file still says '## 0.2.0 (unreleased)'. Only the
# base TIP knows 0.2.0 has been stamped.
#
# This case used to be asserted here as a PASS ('branch behind a release cut'), on the
# reading that a branch merely behind main must not be failed. That reading is right
# about a branch behind main (the three cases below) and wrong about this one.
forkcase fail 'entry into a section stamped since fork' \
	"$SYNTH_BASE" "$FORK_BASE_RELEASED" "$FORK_BRANCH_ENTRY"

# The fix for the case above, and the shape a fresh branch has anyway: the entry sits
# under a heading this change added. R4 must not reach for it.
forkcase pass 'branch opened its own unreleased head' \
	"$SYNTH_BASE" "$FORK_BASE_RELEASED" "$FORK_BRANCH_NEW_SECTION"

# The tolerance that must survive, direction one: the base opened a newer section but
# stamped nothing, so the branch's section has NOT shipped. This is the case that fails
# if R4 ever keys on position ("no longer the topmost heading") instead of the stamp.
forkcase pass 'base opened a section, stamped none' \
	"$SYNTH_BASE" "$FORK_BASE_UNSTAMPED" "$FORK_BRANCH_ENTRY"

# The tolerance that must survive, direction two, and the commonest PR there is: the
# base is simply ahead, with another entry in the same still-open section.
forkcase pass 'branch merely behind the base branch' \
	"$SYNTH_BASE" "$FORK_BASE_AHEAD" "$FORK_BRANCH_ENTRY"

# A PR that never touches CHANGELOG.md, while the base released underneath it. The diff
# is empty, so the guard has nothing to judge and must stay out of the way.
forkcase pass 'PR that does not touch the changelog' \
	"$SYNTH_BASE" "$FORK_BASE_RELEASED" "$SYNTH_BASE"

# R4 inherits both of R1's carve-outs, and they carry the same weight here as there.
# A hunk that also DELETES is someone rewriting a line that was already in the section
# — the commonest honest reason to touch it — and cannot be a rebase replaying an entry.
forkcase pass 'typo fixed in a stamped section' \
	"$SYNTH_BASE" "$FORK_BASE_RELEASED" "$FORK_BRANCH_TYPO"

# The other carve-out: a line whose exact text was also deleted in this diff is a move,
# not new content. Reordering two entries inside the section reports an advisory note
# and passes; failing it would red a tidy-up, which is how a guard gets bypassed.
forkcase pass 'entries reordered in a stamped section' \
	"$FORK_TWO_ENTRIES" "$FORK_TWO_RELEASED" "$FORK_TWO_REORDERED"

# The tip scan is fence-aware for the same reason the base scan is (finding 5, above):
# a heading quoted in an example is not a heading. Reading the fenced '## 0.2.0
# (2026-02-01)' as the real one would fail a branch writing into a section that has not
# shipped — a false red on an entry describing this guard.
forkcase pass 'stamped heading quoted at the tip' \
	"$SYNTH_BASE" "$FORK_BASE_FENCED" "$FORK_BRANCH_ENTRY"

# The stamped top heading. This is the case that fails if R4 exempts the base tip's
# FIRST heading — a tempting exemption, since the top heading is normally the open one,
# but a dated top heading means there is no open section and the entry still lands in
# shipped history.
forkcase fail 'entry under a stamped top heading' \
	"$SYNTH_BASE" "$FORK_BASE_STAMPED_TOP" "$FORK_BRANCH_ENTRY"

echo ""

# The invocation itself is load-bearing: without --head the case above fails, and
# without --require-base an unresolvable base prints a skip and a green tick. Neither
# is visible from the guard's own behaviour, so the workflow line is asserted here.
WORKFLOW=.github/workflows/ci.yml
for flag in --head --require-base; do
	total=$((total + 1))
	if grep -q -- "check-changelog-placement.sh .*$flag" "$WORKFLOW"; then
		printf '  ok    %-38s %s\n' "workflow passes $flag" 'present'
	else
		printf '  FAIL  %-38s missing from %s\n' "workflow passes $flag" "$WORKFLOW"
		failed=$((failed + 1))
	fi
done

# Admins bypass branch protection here, so release and pin commits reach main by a
# direct push and never meet the pull_request step. Without this the guard covers only
# the PR route, which is invisible from the guard itself.
total=$((total + 1))
if grep -q "github.event_name == 'push'" "$WORKFLOW" &&
	grep -q -- 'check-changelog-placement.sh --base ..{ github.event.before' "$WORKFLOW"; then
	printf '  ok    %-38s %s\n' 'workflow guards direct pushes' 'present'
else
	printf '  FAIL  %-38s missing from %s\n' 'workflow guards direct pushes' "$WORKFLOW"
	failed=$((failed + 1))
fi

echo ""
if [ "$failed" -ne 0 ]; then
	echo "check-changelog-placement-test: $failed of $total case(s) FAILED"
	exit 1
fi
echo "check-changelog-placement-test: OK ($total/$total cases behave as expected)"
