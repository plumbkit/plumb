package topology

import (
	"context"
	"path/filepath"
	"strings"

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
