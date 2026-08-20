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
// (ignorePattern.matchesPath) is untouched — gitignore genuinely has no brace
// syntax, and giving it one here would silently change which files are ignored.

const (
	// maxBraceExpansions bounds the combinatorial blow-up of nested groups:
	// "{a,b}{c,d}{e,f}..." doubles per group. Refused rather than truncated —
	// a silently shortened alternative list is the same class of bug this
	// whole change exists to remove.
	maxBraceExpansions = 256
	// maxBraceDepth bounds nesting independently of the total, so a pathological
	// pattern is rejected before it is expanded rather than after.
	maxBraceDepth = 10
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

// findBraceGroup locates the first brace group and its MATCHING close brace,
// tracking depth so a nested group does not terminate its parent early.
// ok is false when there is no group, or the braces are unbalanced.
func findBraceGroup(pattern string) (start, end int, ok bool) {
	start = strings.IndexByte(pattern, '{')
	if start < 0 {
		return 0, 0, false
	}
	depth := 0
	for i := start; i < len(pattern); i++ {
		switch pattern[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return start, i, true
			}
		}
	}
	return 0, 0, false // unbalanced — caller treats the pattern as literal
}

// splitTopLevelCommas splits on commas at brace depth zero, so the comma inside
// "{a,{b,c}}" belongs to the inner group and does not split the outer one.
func splitTopLevelCommas(inner string) []string {
	var parts []string
	depth, last := 0, 0
	for i := range len(inner) {
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
	}
	parts = append(parts, inner[last:])
	return parts
}
