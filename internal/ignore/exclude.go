package ignore

// exclude.go holds the matching rule for [topology] exclude_patterns and the
// guard that decides which patterns may be honoured at all.
//
// Both live here, next to DoubleStarMatch, for one reason: the guard probes
// patterns THROUGH the same matcher the real filter runs, so it cannot drift
// from the behaviour it guards. The rule it replaced was a literal denylist of
// six spellings, and four trivial respellings walked past it — "**/**",
// "**/**/**", "**/**/*" and "?*" each match every path in a workspace and each
// emptied a real index end to end.
//
// The other reason is the caller set: internal/topology applies the patterns,
// and internal/config must refuse a bad one on the WRITE path (an agent that
// sets exclude_patterns = ["**"] has to be told, not warned about in a log it
// never reads). internal/config cannot import internal/topology, so the shared
// half belongs in this foundation package.

import (
	"path"
	"path/filepath"
	"strings"
)

// MatchExcludePattern reports whether one exclude pattern matches one
// workspace-relative path.
//
// The rule is gitignore's, minus the negation and directory-only syntax: a
// pattern containing "/" is matched against the whole relative path, and one
// without is matched against the base name at any depth. "**" spans path
// components.
func MatchExcludePattern(pattern, rel string) bool {
	rel = filepath.ToSlash(rel)
	if rel == "" || rel == "." {
		return false
	}
	if DoubleStarMatch(pattern, rel) {
		return true
	}
	return !strings.Contains(pattern, "/") && DoubleStarMatch(pattern, path.Base(rel))
}

// excludeProbePaths is the synthetic workspace the catch-all guard tests a
// pattern against: a top-level directory, a nested one, a deep file, and a
// top-level file. A pattern that matches ALL of them matches everything a real
// walk can present, whatever spelling it used to say so.
//
// The shapes are what matter, not the names. A top-level FILE is the one that
// does the work — it is what separates "**" (which matches it, and empties the
// index) from "*/**" (which does not).
var excludeProbePaths = []string{"a", "a/b", "a/b/c/d.go", "main.go"}

// excludeWildcardChars are the characters that carry no information about WHICH
// tree a pattern names. A pattern built from nothing else asks to exclude by
// shape alone.
const excludeWildcardChars = "*?/"

// ExcludePatternRefusal returns the reason an exclude pattern must not be
// honoured, or "" when it is fine. The reason is a phrase, for a caller to
// splice into its own message.
//
// Two refusals, both decided by running the pattern through
// MatchExcludePattern rather than by recognising a spelling:
//
//   - It matches every path. "**", "**/**", "**/**/*" and "?*" all do, and all
//     of them would prune every directory on the next resync and let the prune
//     pass delete every row — every topology tool then answers "nothing found",
//     successfully, with no error anywhere. A caller that genuinely wants no
//     index has [topology] enabled = false, which says so.
//   - It is built from wildcards and separators alone. "*/**", "./**" and
//     "/**/" name no particular tree: under this matcher they either match
//     everything (caught above) or, as it happens today, match nothing at all.
//     Keeping one silently would leave the author believing an exclusion is in
//     force that never fires, which is the "write succeeded, nothing happened"
//     failure this whole field exists to have fixed.
func ExcludePatternRefusal(pattern string) string {
	p := normaliseExcludePattern(pattern)
	if p == "" {
		return "it is empty"
	}
	all := true
	for _, probe := range excludeProbePaths {
		if !MatchExcludePattern(p, probe) {
			all = false
			break
		}
	}
	if all {
		return "it matches every path in the workspace, which would empty the index; " +
			"use [topology] enabled = false to turn the index off"
	}
	if strings.Trim(p, excludeWildcardChars) == "" {
		return "it is made of wildcards alone and so names no particular tree; " +
			"name the directory or file shape to exclude, e.g. \"third_party/**\" or \"*.pb.go\""
	}
	return ""
}

// normaliseExcludePattern reduces the spellings of a workspace-root-relative
// pattern to one, so "./**" and "/**/" are judged as the "**" they mean rather
// than sneaking past a check on their literal text. "." — the workspace root
// itself — reduces to the empty pattern.
func normaliseExcludePattern(pattern string) string {
	p := filepath.ToSlash(strings.TrimSpace(pattern))
	for {
		switch {
		case strings.HasPrefix(p, "./"):
			p = p[2:]
		case strings.HasPrefix(p, "/"):
			p = p[1:]
		default:
			p = strings.TrimSuffix(p, "/")
			if p == "." {
				return ""
			}
			return p
		}
	}
}
