// Package tokenise provides identifier tokenisation shared by the topology and
// memory FTS indexes. It is a leaf package — it imports nothing project-local,
// so both sibling indexers can depend on it without a layering cycle.
package tokenise

import (
	"strings"
	"unicode"
)

// SplitIdentifier converts an identifier to a space-separated lowercase token
// string for FTS5 indexing. It handles camelCase, PascalCase, snake_case,
// kebab-case, dotted, and slashed identifiers so "workspacePool" matches a query
// for "workspace pool" and "UserSession" matches "user session". An empty input
// returns an empty string.
func SplitIdentifier(s string) string {
	if s == "" {
		return ""
	}
	runes := []rune(s)
	var buf strings.Builder
	buf.Grow(len(s) + len(s)/4)
	started := false // at least one kept rune has been emitted
	pending := false // a separator was seen; emit one space before the next kept rune
	for i, r := range runes {
		if isSeparator(r) {
			if started {
				pending = true // collapses runs; never leading (started is false first)
			}
			continue
		}
		// Case-boundary space — only when the previous rune was itself kept. A
		// preceding separator already forces the break via pending, mirroring the
		// old implementation's "no boundary space when the prior char is a space".
		if started && !pending {
			prev := runes[i-1]
			// Split on a lower→upper boundary: "workspacePool" → "workspace pool".
			lowerToUpper := unicode.IsUpper(r) && !unicode.IsUpper(prev)
			// Split before the last uppercase of a consecutive-uppercase run when the
			// next letter is lowercase: "HTTPServer" → "http server".
			upperSeqToLower := unicode.IsUpper(r) && unicode.IsUpper(prev) &&
				i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if lowerToUpper || upperSeqToLower {
				buf.WriteRune(' ')
			}
		}
		if pending {
			buf.WriteRune(' ')
			pending = false
		}
		buf.WriteRune(unicode.ToLower(r))
		started = true
	}
	return buf.String()
}

// isSeparator reports whether r breaks an identifier into tokens: the explicit
// snake/kebab/dotted/slashed separators, plus any Unicode whitespace (which the
// previous implementation's terminal strings.Fields collapsed and trimmed).
func isSeparator(r rune) bool {
	switch r {
	case '_', '-', '.', '/':
		return true
	}
	return unicode.IsSpace(r)
}
