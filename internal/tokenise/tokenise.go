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
	s = strings.NewReplacer("_", " ", "-", " ", ".", " ", "/", " ").Replace(s)
	var buf strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		if i > 0 && runes[i-1] != ' ' {
			// Split on a lower→upper boundary: "workspacePool" → "workspace pool".
			lowerToUpper := unicode.IsUpper(r) && !unicode.IsUpper(runes[i-1])
			// Split before the last uppercase letter of a consecutive-uppercase run
			// when the next letter is lowercase: "HTTPServer" → "http server".
			upperSeqToLower := unicode.IsUpper(r) && unicode.IsUpper(runes[i-1]) &&
				i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if lowerToUpper || upperSeqToLower {
				buf.WriteRune(' ')
			}
		}
		buf.WriteRune(unicode.ToLower(r))
	}
	return strings.Join(strings.Fields(buf.String()), " ")
}
