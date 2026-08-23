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

// CallSiteExtractor is the optional half of Extractor: an extractor that also
// records raw, unresolved call sites. It is one method rather than a second
// Extract call so a file is parsed once — the nodes, the edges and the sites all
// come out of the same parse.
//
// An extractor that does not implement it contributes no call sites, and the
// resolver's language admission (see callgraph.go) keeps that from being read as
// "this language has no calls".
type CallSiteExtractor interface {
	Extractor
	// ExtractWithCallSites parses src and returns what Extract returns, plus the
	// call sites found in it. Implementations must keep Extract and this method
	// in agreement on nodes and edges: CallSite.EnclosingIdx indexes the returned
	// nodes slice.
	ExtractWithCallSites(ctx context.Context, path string, src []byte) ([]Node, []Edge, []CallSite, error)
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
