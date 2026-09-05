package tools

import (
	"fmt"
	"strings"
)

// Brace alternation for glob patterns.
//
// filepath.Match — which every glob path in this package bottoms out in — has
// no brace syntax: it treats `{`, `}` and `,` as ordinary literal characters
// and returns NO error for them. So "*.{ts,js}" validated cleanly, matched
// nothing, and answered "No files found" with no indication why. search_in_files
// had already refused braces outright for this reason; find_files and
// find_replace still failed silently, and plumb's own diagnostics scan carried a
// documented per-extension workaround because of it.
//
// Expansion happens one layer above filepath.Match so that .gitignore matching
// is untouched — gitignore genuinely has no brace syntax, and giving it one
// here would silently change which files are ignored. That is now structural
// rather than a convention: the matcher lives in internal/ignore, which cannot
// see this file. See TestGitignore_BracesStayLiteral there.
//
// A brace can be ESCAPED to keep it literal: `notes\{draft,final\}.md` matches
// the file of that exact name. Without an opt-out, a pattern that relied on
// braces being ordinary characters — which they were before this existed — would
// silently start matching different files: the same class of silent wrong answer
// brace support was added to remove. filepath.Match already treats a backslash
// as an escape, so the escaped brace passes straight through to it.

const (
	// maxBraceExpansions bounds the combinatorial blow-up of nested groups:
	// "{a,b}{c,d}{e,f}..." doubles per group. Refused rather than truncated —
	// a silently shortened alternative list is the same class of bug this
	// whole change exists to remove.
	maxBraceExpansions = 256
	// maxBraceDepth bounds nesting independently of the total, so a pathological
	// pattern is rejected before it is expanded rather than after.
	maxBraceDepth = 10
	// maxBraceGroups bounds the NUMBER of groups, counted before any recursion.
	// The other two caps constrain real alternation only, so a long run of
	// comma-less groups ("{x}{x}{x}…") slipped past both and cost O(n²) — 200k
	// groups took ~20s — and doubleStarMatchFile re-expands once per file visited,
	// multiplying that by the file count of a walk. Counting first makes the
	// refusal O(n) and immediate.
	maxBraceGroups = 32
)

// expandBraces expands shell-style brace alternation into the concrete glob
// patterns it denotes: "*.{ts,tsx}" becomes ["*.ts", "*.tsx"]. Groups may nest
// ("src/{a,b{c,d}}/*.go") and a pattern may hold several.
//
// A pattern with no braces returns itself, so callers can expand
// unconditionally. Two shapes are deliberately left as literal text, matching
// shell behaviour: an unbalanced brace ("a{b"), and a group with no top-level
// comma ("{x}"), which the shell also does not expand.
func expandBraces(pattern string) ([]string, error) {
	if !strings.ContainsAny(pattern, "{}") {
		return []string{pattern}, nil
	}
	if n := countBraceGroups(pattern); n > maxBraceGroups {
		return nil, fmt.Errorf("glob %q contains %d brace groups; the maximum is %d", pattern, n, maxBraceGroups)
	}
	return expandBracesDepth(pattern, 0)
}

func expandBracesDepth(pattern string, depth int) ([]string, error) {
	if depth > maxBraceDepth {
		return nil, fmt.Errorf("glob %q nests brace groups more than %d deep", pattern, maxBraceDepth)
	}
	start, end, ok := findBraceGroup(pattern)
	if !ok {
		return []string{pattern}, nil
	}
	prefix, inner, suffix := pattern[:start], pattern[start+1:end], pattern[end+1:]

	alts := splitTopLevelCommas(inner)
	if len(alts) < 2 {
		// "{x}" is literal in the shell too. Keep the braces and carry on
		// scanning the remainder for a real group.
		rest, err := expandBracesDepth(suffix, depth)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(rest))
		for _, r := range rest {
			out = append(out, prefix+"{"+inner+"}"+r)
		}
		return out, nil
	}

	tails, err := expandBracesDepth(suffix, depth)
	if err != nil {
		return nil, err
	}

	var out []string
	for _, alt := range alts {
		heads, err := expandBracesDepth(prefix+alt, depth+1)
		if err != nil {
			return nil, err
		}
		for _, h := range heads {
			for _, tl := range tails {
				if len(out) >= maxBraceExpansions {
					return nil, fmt.Errorf(
						"glob %q expands to more than %d alternatives; narrow it or run separate calls",
						pattern, maxBraceExpansions)
				}
				out = append(out, h+tl)
			}
		}
	}
	return out, nil
}

// countBraceGroups counts opening braces that are not escaped. A plain
// strings.Count would also count `\{`, so a pattern built entirely of ESCAPED
// groups — which by design expands to nothing and costs nothing — could trip the
// cap meant for the expensive unescaped kind.
func countBraceGroups(pattern string) int {
	n := 0
	for i := 0; i < len(pattern); {
		if pattern[i] == '\\' {
			i += 2
			continue
		}
		if pattern[i] == '{' {
			n++
		}
		i++
	}
	return n
}

// findBraceGroup locates the first UNESCAPED brace group and its MATCHING close
// brace, tracking depth so a nested group does not terminate its parent early.
// ok is false when there is no group, or the braces are unbalanced.
func findBraceGroup(pattern string) (start, end int, ok bool) {
	start, depth := -1, 0
	for i := 0; i < len(pattern); {
		if pattern[i] == '\\' {
			i += 2 // skip the escaped byte; filepath.Match unescapes it later
			continue
		}
		switch pattern[i] {
		case '{':
			if start < 0 {
				start = i
			}
			depth++
		case '}':
			if start >= 0 {
				depth--
				if depth == 0 {
					return start, i, true
				}
			}
		}
		i++
	}
	return 0, 0, false // no group, or unbalanced — the caller treats it as literal
}

// splitTopLevelCommas splits on UNESCAPED commas at brace depth zero, so the
// comma inside "{a,{b,c}}" belongs to the inner group and does not split the
// outer one, and an escaped "\," is data rather than a separator.
func splitTopLevelCommas(inner string) []string {
	var parts []string
	depth, last := 0, 0
	for i := 0; i < len(inner); {
		if inner[i] == '\\' {
			i += 2
			continue
		}
		switch inner[i] {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, inner[last:i])
				last = i + 1
			}
		}
		i++
	}
	parts = append(parts, inner[last:])
	return parts
}
