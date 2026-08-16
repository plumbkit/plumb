#!/bin/sh
# check-changelog-placement.sh — catch a CHANGELOG entry added under the wrong heading.
#
# Why: CHANGELOG.md keeps a topmost '## <version> (unreleased)' heading that new
# entries belong under, and a clean rebase will happily replay an addition under an
# already-released heading further down — git sees new lines, so it never conflicts,
# and nobody checks where they landed. That has cost a by-hand relocation four times
# (53bb60a4 for PR #320, 3fa005e7 for #292, 3e885aca for #310, and f9451a42, which
# appended a second '## 0.16.6' heading to main and needed #325 to unpick). The file
# has carried an HTML comment warning about it since 0.16.5; a comment is not a guard.
#
# scripts/check-changelog-headings.sh is the whole-file half of the pair — no version
# number may appear in two headings. This is the diff-shape half: where did the ADDED
# lines actually land. Neither subsumes the other; #320/#292/#310 created no duplicate
# heading at all, and a duplicate can arrive by a route this script never sees.
#
# Two rules, over `git diff` against a real merge-base. Only lines added by a
# PURE-ADDITION hunk count: deleting from an old section, or rewriting a line that
# was already there, is always fine, because a rebase replays entries rather than
# editing existing ones (see the pass-1 comment on the minus side of '@@').
#
#   R1  A non-blank added line's enclosing '## ' heading must be the BASE's first
#       heading (matched on the version number, so a release re-stamp from
#       '(unreleased)' to a date still matches), or a heading this change added itself.
#   R2  An added '## ' heading must have no pre-existing '## ' heading above it, i.e.
#       added headings form a contiguous run from the top of the file. R2 is what
#       makes R1's "a heading we added" clause safe, and what catches f9451a42.
#
# Why the BASE's first heading and not the new file's: when the diff range spans a
# release cut, the old unreleased heading is no longer topmost, but entries added
# under it were correct when they were written. Comparing against the new file would
# fail every branch that is merely behind main.
#
# Why blank added lines are ignored: a whitespace-only change inside an old section —
# reflowing a paragraph, separating two bullets — is not a misplaced entry, and
# failing it would be a pure false positive. Note this is NOT what saves
# `chore(release)`: the blank line a release adds after the newly-dated heading sits
# under a heading that same commit added, which R1 already allows. Established by
# deleting this rule and re-running the suite — every real case still passed, so the
# rule is covered by a synthetic case in check-changelog-placement-test.sh instead.
#
# Why it anchors on "the first '## ' line" and never greps for '(unreleased)': there
# are 50+ historical '(unreleased)' headings in this file, from an era when releases
# were not date-stamped.
#
# CI-ONLY, DELIBERATELY. Unlike its sibling guards (check-file-size.sh,
# check-agents-brief.sh, check-changelog-headings.sh) this one is NOT in `make verify`
# and NOT in the pre-commit hook. Those are whole-file invariants; this one needs a
# base ref, and a working tree does not have one. `make verify` has to stay hermetic —
# offline, and correct in a fresh or shallow clone — and the hook has to stay fast and
# base-agnostic. It runs instead as a step in CI's `verify` job, which already checks
# out with fetch-depth: 0 and can hand it the exact base SHA from the pull_request
# event. `make check-changelog-placement` runs it on demand locally against origin/main.
#
# HARD FAIL vs ADVISORY. A false positive is worse than a false negative here: a guard
# that blocks a legitimate changelog cleanup gets bypassed, and a bypassed guard guards
# nothing. So ONLY R1 and R2 fail. Everything uncertain prints a note and exits 0 — no
# resolvable base ref, CHANGELOG.md absent from the base, no '## ' heading in the base,
# an empty diff, and an added line whose exact text was also DELETED in the same diff.
# That last one is a relocation rather than new content, and it cannot hide a bad
# rebase, because a bad rebase adds without deleting (f9451a42's numstat was 52 0).
# The consequence worth naming: a PURE relocation into a released section passes,
# because a deliberate move and a botched one are the same diff. Those lines are
# reported as an advisory note so a reviewer still sees them — 3e885aca is the worked
# example, where 16 lines moved into the wrong section and only the one genuinely new
# line ('### Added') failed the check.
#
# Last resort: CHANGELOG_PLACEMENT_ALLOW=1, meant to be set on the workflow step in
# the same PR, so the bypass is part of the diff a reviewer reads, not a deletion.
set -eu

usage() {
	cat <<'EOF'
usage: check-changelog-placement.sh [--base <ref>] [--head <ref>] [--require-base]

  --base <ref>    compare against the merge-base with <ref>
                  (default: $CHANGELOG_BASE_REF, else origin/main)
  --head <ref>    check <ref> instead of the working tree. This is how a historical
                  commit is replayed: --base <sha>^ --head <sha>
  --require-base  exit 1 instead of skipping when the base cannot be resolved.
                  For CI, where a skip is a green lie; leave it off locally.

environment:
  CHANGELOG_BASE_REF      default for --base
  CHANGELOG_PLACEMENT_ALLOW  set to any value to bypass the guard entirely; meant
                          to be set on the workflow step in the PR that needs it
EOF
}

BASE_REF="${CHANGELOG_BASE_REF:-origin/main}"
HEAD_REF=""
REQUIRE_BASE=0

while [ $# -gt 0 ]; do
	case "$1" in
	--base)
		[ $# -ge 2 ] || {
			usage >&2
			exit 2
		}
		BASE_REF="$2"
		shift 2
		;;
	--base=*)
		BASE_REF="${1#--base=}"
		shift
		;;
	--head)
		[ $# -ge 2 ] || {
			usage >&2
			exit 2
		}
		HEAD_REF="$2"
		shift 2
		;;
	--head=*)
		HEAD_REF="${1#--head=}"
		shift
		;;
	--require-base)
		REQUIRE_BASE=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		echo "check-changelog-placement: unknown argument: $1" >&2
		usage >&2
		exit 2
		;;
	esac
done

skip() {
	echo "check-changelog-placement: skipped — $1"
	exit 0
}

# Why the two exit paths differ: fail-open is right for the local and `make` routes,
# where a fresh or shallow clone genuinely has no base and blocking there just teaches
# people to bypass the guard. It is wrong for the route that gates merges — CI's base
# comes from the event payload and its history from fetch-depth: 0, so a base it cannot
# resolve means the checkout changed under the guard, and skipping prints a pass for a
# check that never ran. That is the "0 of 10 cases ran" green lie the test harness
# header warns about, in the one place it would not be noticed.
needbase() {
	[ "$REQUIRE_BASE" = 1 ] || skip "$1"
	echo "check-changelog-placement: $1" >&2
	echo "check-changelog-placement: --require-base is set, so this is an error rather than a skip." >&2
	exit 1
}

[ -z "${CHANGELOG_PLACEMENT_ALLOW:-}" ] || skip "CHANGELOG_PLACEMENT_ALLOW is set"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

git rev-parse --git-dir >/dev/null 2>&1 || needbase "not a git repository"

HEAD_REV="${HEAD_REF:-HEAD}"
git rev-parse --verify -q "$BASE_REF^{commit}" >/dev/null ||
	needbase "base ref '$BASE_REF' does not resolve (shallow clone, or no remote configured)"
git rev-parse --verify -q "$HEAD_REV^{commit}" >/dev/null ||
	needbase "head ref '$HEAD_REV' does not resolve"

BASE="$(git merge-base "$BASE_REF" "$HEAD_REV")" ||
	needbase "no merge-base between '$BASE_REF' and '$HEAD_REV'"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT INT TERM

git cat-file -e "$BASE:CHANGELOG.md" 2>/dev/null ||
	skip "CHANGELOG.md does not exist at the base ($BASE)"
git show "$BASE:CHANGELOG.md" >"$TMP/base.md"

if [ -n "$HEAD_REF" ]; then
	git show "$HEAD_REF:CHANGELOG.md" >"$TMP/new.md" 2>/dev/null ||
		skip "CHANGELOG.md does not exist at '$HEAD_REF'"
	git diff --no-ext-diff --unified=0 --no-color "$BASE" "$HEAD_REF" -- CHANGELOG.md >"$TMP/diff"
else
	[ -f CHANGELOG.md ] || skip "no CHANGELOG.md in the working tree"
	cp CHANGELOG.md "$TMP/new.md"
	# --no-ext-diff on both paths: a user's diff.external or GIT_EXTERNAL_DIFF emits
	# no unified diff, which would read here as "CHANGELOG.md is unchanged" and pass.
	git diff --no-ext-diff --unified=0 --no-color "$BASE" -- CHANGELOG.md >"$TMP/diff"
fi

BASE_FIRST="$(awk '/^## /{print; exit}' "$TMP/base.md")"
[ -n "$BASE_FIRST" ] || skip "the base CHANGELOG.md has no '## ' heading"
[ -s "$TMP/diff" ] || skip "CHANGELOG.md is unchanged against ${BASE}"

# Pass 1 reads the unified=0 diff and records which NEW-file line numbers were added
# (plus a multiset of deleted line texts, for relocation detection). Pass 2 reads the
# post-change file, so every added line can be mapped to the heading it landed under.
awk -v basefirst="$BASE_FIRST" -v base="$BASE" '
# Relocated lines never fail the check, but they are reported either way: a move into
# a released section is legitimate for a cleanup and a mistake for a rebase, and only
# a human can tell which.
function relocnote(   i) {
	if (!nmoved) return
	printf "  note: %d line(s) were RELOCATED into an already-released section — their\n", nmoved
	print "        exact text was also deleted in this diff, so this is a move, not a new"
	print "        entry. Advisory, never a failure; confirm it was deliberate:"
	for (i = 1; i <= nmt; i++) printf "          %d line(s) under %s\n", mcount[i], mhead[i]
	print ""
}

function version(h) {
	if (match(h, /^## [0-9]+\.[0-9]+\.[0-9]+/)) {
		return substr(h, 4, RLENGTH - 3)
	}
	return h
}

FNR == NR {
	if ($0 ~ /^@@/) {
		# "@@ -a,b +c,d @@" — a missing count means exactly one line, and d == 0
		# marks a pure-deletion hunk. Anything before the first @@ is file header
		# noise, which is why "---"/"+++" need no special case.
		inhunk = 1
		plus = $3
		sub(/^\+/, "", plus)
		n = split(plus, p, ",")
		cur = p[1] + 0
		rem = (n > 1) ? p[2] + 0 : 1
		# The minus side decides whether this hunk REWRITES or purely INSERTS, and
		# only an insertion can be a misplaced entry: a rebase replays entries, it
		# never edits the ones already there. All four incidents confirm it —
		# f9451a42, 53bb60a4 and 3fa005e7 are 52/0, 44/0 and 47/0 in git numstat, and
		# 3e885aca is 17/16 overall but adds through a single pure-addition hunk, with
		# its deletions in separate pure-deletion hunks. A hunk that both
		# adds and deletes is someone rewriting text that was already there — fixing
		# a typo or reflowing a line in a released section — which R1 must not fail,
		# because a guard that blocks a legitimate cleanup gets bypassed.
		minus = $2
		sub(/^-/, "", minus)
		nm = split(minus, m, ",")
		mrem = (nm > 1) ? m[2] + 0 : 1
		next
	}
	if (!inhunk) next
	c = substr($0, 1, 1)
	if (c == "+") {
		if (rem > 0) {
			added[cur] = 1
			if (mrem == 0) pureadd[cur] = 1
			cur++
			rem--
		}
		next
	}
	if (c == "-") deleted[substr($0, 2)]++
	next
}

{
	text[FNR] = $0
	# A heading line inside a fenced code block is quoted text, not a heading — an
	# entry that shows a CHANGELOG heading in an example would otherwise trip R2.
	if (substr($0, 1, 3) == "```") infence = !infence
	if (!infence && $0 ~ /^## /) {
		nh++
		hline[nh] = FNR
		htext[nh] = $0
		hadded[nh] = (FNR in added) ? 1 : 0
		if (!hadded[nh] && !firstpre) firstpre = nh
	}
	enclosing[FNR] = nh
	if (FNR in added) order[++na] = FNR
}

END {
	status = 0
	basever = version(basefirst)

	# R2 — an added heading below a pre-existing one.
	for (i = 1; i <= nh; i++) {
		if (!hadded[i] || !firstpre || i <= firstpre) continue
		status = 1
		nv++
		vline[nv] = hline[i]
		vtext[nv] = htext[i]
	}

	# R1 — an added content line under a section that is neither newly added nor the
	# one that was unreleased at the base.
	for (k = 1; k <= na; k++) {
		ln = order[k]
		t = text[ln]
		if (t ~ /^[ \t]*$/) continue
		if (t ~ /^## /) continue
		h = enclosing[ln]
		if (h == 0) continue
		if (hadded[h]) continue
		if (version(htext[h]) == basever) {
			checked++
			continue
		}
		if (!(ln in pureadd)) continue
		if (deleted[t] > 0) {
			deleted[t]--
			nmoved++
			if (!(htext[h] in mseen)) {
				mseen[htext[h]] = ++nmt
				mhead[nmt] = htext[h]
			}
			mcount[mseen[htext[h]]]++
			continue
		}
		status = 1
		# Collapse consecutive lines under the same heading into one range.
		if (nb > 0 && bhead[nb] == htext[h] && bend[nb] == ln - 1) {
			bend[nb] = ln
		} else {
			nb++
			bstart[nb] = ln
			bend[nb] = ln
			bhead[nb] = htext[h]
		}
	}

	if (!status) {
		if (checked) printf "check-changelog-placement: OK (%d added line(s), all under %s)\n", checked, basefirst
		else if (nmoved) print "check-changelog-placement: OK (nothing NEW was added to a released section)"
		else print "check-changelog-placement: OK (nothing was added to a released section)"
		relocnote()
		exit 0
	}

	print "check-changelog-placement: CHANGELOG.md additions landed under the wrong heading."
	print ""
	printf "  base:                %s\n", base
	printf "  unreleased section:  %s\n", basefirst
	print ""
	if (nv) {
		print "  A new version heading was added BELOW an existing one — a release heading"
		printf "  belongs at the top of the file (%s is at line %d):\n", htext[firstpre], hline[firstpre]
		for (i = 1; i <= nv; i++) printf "    line %d: %s\n", vline[i], vtext[i]
		print ""
	}
	if (nb) {
		print "  Lines added under an already-released section:"
		for (i = 1; i <= nb; i++) {
			if (bstart[i] == bend[i]) printf "    line %d", bstart[i]
			else printf "    lines %d-%d", bstart[i], bend[i]
			printf "  under %s\n", bhead[i]
		}
		print ""
	}
	relocnote()
	print "Move them under the topmost \"## \" heading, the unreleased section. A clean"
	print "rebase is NOT evidence an entry is in the right place: date-stamping a release"
	print "does not conflict with a branch that adds entries under the stamped heading,"
	print "so the insertion applies silently in the wrong section."
	print ""
	print "If the placement really is deliberate — a changelog cleanup, or an entry that"
	print "belongs to a version that already shipped — say so in the PR and set"
	print "CHANGELOG_PLACEMENT_ALLOW=1 on the workflow step, so the bypass is part of the"
	print "diff a reviewer reads."
	exit 1
}
' "$TMP/diff" "$TMP/new.md"
