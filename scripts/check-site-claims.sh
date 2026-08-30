#!/bin/sh
# check-site-claims.sh — fail when site copy's claims drift from the code.
#
# Why: the same three drift classes were fixed twice (the June production
# review, then again in PR #429) before a guard existed. The web ui freshness
# check cannot see them because they are claims-VS-CODE drift, not stale-file
# drift: the site is current, it just says something the code no longer does.
# See plumb-ops board card PLAN-420. Five greppable pairs, no rendering:
#
#   1. footer version stamp vs the VERSION file
#   2. hero languages count vs the extractor-case table
#   3. tool-grid ×N sums vs docs/tools.md and vs the site's own tool stat
#   4. no Validated adapter described as experimental/beta in site copy
#   5. no phantom CLI subcommand in code-context site copy
#
# Runs from make verify, not the pre-commit hook: check 5 shells out to the
# module once, and the hook must stay fast.
set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SITE="$ROOT/site/index.html"
fail=0

bail() {
	echo "check-site-claims: FAIL: $1" >&2
	fail=1
}

# 1. Footer stamp vs VERSION. The footer line is the only version-shaped
# string in the page today; anchor to its wording so a stray version in
# prose cannot mask a stale stamp. This is the claim that has drifted twice.
version=$(tr -d '[:space:]' < "$ROOT/VERSION")
stamp=$(sed -n 's/.*set in Australian English · v\([0-9][0-9.]*\)<\/span>.*/\1/p' "$SITE")
if [ -z "$stamp" ]; then
	bail "no footer version stamp (looked for 'set in Australian English · v…')"
elif [ "$stamp" != "$version" ]; then
	bail "footer stamp v$stamp != VERSION $version — the stamp is bumped by hand per release"
fi

# 2. Hero languages stat vs the extractor table. Each entry in
# allExtractorCases declares exactly one constructor, so counting
# constructors counts entries.
claimed_langs=$(sed -n 's/.*data-count="\([0-9][0-9]*\)">0<\/div><div class="l">languages.*/\1/p' "$SITE")
actual_langs=$(sed -n '/func allExtractorCases/,/^}/p' \
	"$ROOT/internal/topology/extractors/treesitter/all_extractors_test.go" \
	| grep -c 'func() topology.Extractor') || actual_langs=""
if [ -z "$claimed_langs" ]; then
	bail "no languages count found in the hero stats"
elif [ "$claimed_langs" != "$actual_langs" ]; then
	bail "languages stat $claimed_langs != $actual_langs entries in allExtractorCases"
fi

# 3. Tool counts: the hero stat, docs/tools.md, and the tool-grid ×N sums
# must all agree. × appears nowhere else on the page, so the page-wide grep
# is safe. Half-updated pages (grid bumped, stat not) are the drift shape
# that recurred, so the three-way agreement is the point.
docs_tools=$(sed -n 's/.*exposes \*\*\([0-9][0-9]*\)\*\* structured tools.*/\1/p' "$ROOT/docs/tools.md")
site_tools=$(sed -n 's/.*data-count="\([0-9][0-9]*\)">0<\/div><div class="l">structured tools.*/\1/p' "$SITE")
grid_sum=$(grep -o '×[0-9][0-9]*' "$SITE" | tr -d '×' | awk '{s+=$1} END {print s+0}')
if [ -z "$docs_tools" ] || [ -z "$site_tools" ]; then
	bail "could not read the tool count from docs/tools.md or the hero stats"
elif [ "$grid_sum" != "$docs_tools" ] || [ "$grid_sum" != "$site_tools" ]; then
	bail "tool-grid ×N sums to $grid_sum; docs/tools.md says $docs_tools, hero stat says $site_tools"
fi

# 4. Tier claims. Every shipped adapter is Validated today
# (docs/adding-an-lsp.md status table); none may be described as
# experimental/beta in site copy. Checked line-scoped: a mention split
# across tags on different lines can slip past, which the release reviewer
# still owns — the guard makes the common case unrepeatable, like
# TestAdapterCatalogueTiersMatchTheDocumentedStatus does for the catalogue.
for adapter in $(sed -n 's/^| `\([a-z0-9-]*\)` *| .* \*\*Validated.*/\1/p' "$ROOT/docs/adding-an-lsp.md"); do
	if grep -i "$adapter" "$SITE" | grep -qiE 'experimental|beta'; then
		bail "site copy describes Validated adapter '$adapter' as experimental/beta"
	fi
done

# 5. Phantom subcommands. Every code-context `plumb <verb>` mention in site
# copy must name a subcommand the binary actually registers. Only mentions
# introduced by <code>, a backtick, or a $ prompt are claims about the CLI;
# prose ("plumb gives agents…") is deliberately out of scope.
# NO_COLOR plus an ANSI strip: cobra's help is colourised on a tty and the
# colour prefixes would otherwise corrupt the command column.
allowed=$(cd "$ROOT" && NO_COLOR=1 go run ./cmd/plumb --help 2>/dev/null \
	| sed "s/$(printf '\033')\\[[0-9;]*m//g" \
	| sed -n '/^Available Commands:/,/^$/p' \
	| awk 'NF > 1 && $1 != "Available" {print $1}' | sort -u)
if [ -z "$allowed" ]; then
	bail "could not enumerate subcommands from 'go run ./cmd/plumb --help' — is the module buildable?"
fi
mentions=$(cat "$SITE" "$ROOT"/site/blog/posts/*.md 2>/dev/null \
	| grep -oE '(<code[^>]*>|`|\$) ?plumb [a-z][a-z-]*' \
	| sed 's/.*plumb //' | sort -u) || mentions=""
for verb in $mentions; do
	if ! printf '%s\n' "$allowed" | grep -qx -- "$verb"; then
		bail "site copy references 'plumb $verb' — no such subcommand"
	fi
done

if [ "$fail" -ne 0 ]; then
	echo "check-site-claims: site copy has drifted from the code — fix the claims above" >&2
	exit 1
fi
echo "check-site-claims: OK (stamp $version, $claimed_langs languages, $grid_sum tools, $(printf '%s\n' "$mentions" | grep -c . || true) command mentions)"
