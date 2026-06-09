package topology

import (
	"context"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/plumbkit/plumb/internal/langsupport"
)

// Extractor parses source files and returns nodes and edges.
// Implementations must be stateless and safe for concurrent use.
type Extractor interface {
	// Language returns the canonical language name (e.g. "go", "python").
	Language() string
	// Extensions returns file extensions this extractor handles (e.g. ".go").
	Extensions() []string
	// Extract parses src (content of the file at workspace-relative path).
	// Returns (nil, nil, nil) for files that cannot be parsed or should be skipped.
	Extract(ctx context.Context, path string, src []byte) ([]Node, []Edge, error)
}

// splitIdentifier converts an identifier to a space-separated token string for
// FTS5 indexing. Handles camelCase, PascalCase, snake_case, and kebab-case so
// "workspacePool" matches queries for "workspace pool".
func splitIdentifier(s string) string {
	if s == "" {
		return ""
	}
	s = strings.NewReplacer("_", " ", "-", " ", ".", " ", "/", " ").Replace(s)
	var buf strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		if i > 0 && runes[i-1] != ' ' {
			// Split on lower→upper boundary: "workspacePool" → "workspace pool"
			lowerToUpper := unicode.IsUpper(r) && !unicode.IsUpper(runes[i-1])
			// Split before the last uppercase letter of a consecutive-uppercase run when
			// the following letter is lowercase: "HTTPServer" → "http server".
			// Condition: current=upper, previous=upper, next=lower.
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

// findExtractor returns the first Extractor whose patterns match relPath, or nil.
// A pattern is either a dot-prefixed extension (".go") matched against the file
// extension, or a bare filename stem ("dockerfile") matched against the basename
// exactly or as a dotted prefix/suffix ("Dockerfile", "Dockerfile.prod",
// "prod.dockerfile") — so extensionless files are still recognised.
func findExtractor(relPath string, exts []Extractor) Extractor {
	ext := strings.ToLower(filepath.Ext(relPath))
	base := strings.ToLower(filepath.Base(relPath))
	for _, e := range exts {
		for _, pat := range e.Extensions() {
			if langsupport.MatchExtPattern(pat, ext, base) {
				return e
			}
		}
	}
	return nil
}
