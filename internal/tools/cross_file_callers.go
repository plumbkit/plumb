package tools

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// CallerSite is a single reference to a symbol from another file: a workspace
// path and a 1-based line number.
type CallerSite struct {
	Path string
	Line int
}

// CrossFileCallersFunc resolves the cross-file caller sites of the symbol named
// `name` defined in `path`. It returns only references that live OUTSIDE `path`
// (intra-file callers are already covered by the topology call graph). It is
// best-effort: nil when no language server is wired or the lookup fails.
//
// This exists because the Go topology extractor records call edges intra-file
// only (single-file extraction has no cross-file symbol table), so
// topology_impact's inward section misses callers in other files/packages. The
// language server resolves them accurately, so the daemon fills the gap rather
// than leaving the agent to run a second find_references call.
type CrossFileCallersFunc func(ctx context.Context, path, name string) []CallerSite

// NewLSPCrossFileCallers builds a CrossFileCallersFunc backed by the language
// server. It resolves the symbol's identifier position via the DocumentSymbol
// SelectionRange (the same position get_definition/find_references query),
// requests its references, and keeps those outside the symbol's own file.
// Returns nil when client is nil.
func NewLSPCrossFileCallers(client lsp.Client, c *cache.Cache, ttl, timeout time.Duration) CrossFileCallersFunc {
	if client == nil {
		return nil
	}
	return func(ctx context.Context, path, name string) []CallerSite {
		if name == "" || path == "" {
			return nil
		}
		uri := toFileURI(path)

		ctx, cancel := withLSPDeadline(ctx, timeout)
		defer cancel()

		syms := cachedDocumentSymbols(ctx, client, c, ttl, uri)
		matches := resolveSymbolsByName(syms, name)
		if len(matches) == 0 {
			return nil
		}

		locs, err := client.References(ctx, protocol.ReferenceParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri},
			Position:     matches[0].SelectionRange.Start,
			Context:      protocol.ReferenceContext{IncludeDeclaration: false},
		})
		if err != nil {
			return nil
		}
		return crossFileSites(locs, uri)
	}
}

// cachedDocumentSymbols returns the document symbols for uri, reusing the
// session cache under the shared ":docSymbols" key when one is supplied. Errors
// (cold or absent server) collapse to nil — callers treat this as best-effort.
func cachedDocumentSymbols(ctx context.Context, client lsp.Client, c *cache.Cache, ttl time.Duration, uri string) []protocol.DocumentSymbol {
	key := uri + ":docSymbols"
	if c != nil {
		if v, ok := c.Get(key); ok {
			if syms, ok := v.([]protocol.DocumentSymbol); ok {
				return syms
			}
		}
	}
	syms, err := client.DocumentSymbols(ctx, protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
	})
	if err != nil {
		return nil
	}
	if c != nil {
		c.Set(key, syms, ttl)
	}
	return syms
}

// crossFileSites distils reference locations into distinct caller sites outside
// selfURI, ordered by path then line for deterministic output.
func crossFileSites(locs []protocol.Location, selfURI string) []CallerSite {
	self := strings.TrimPrefix(selfURI, "file://")
	seen := map[string]bool{}
	var out []CallerSite
	for _, l := range locs {
		p := strings.TrimPrefix(l.URI, "file://")
		if p == self {
			continue // intra-file: the topology call graph already covers it
		}
		line := int(l.Range.Start.Line) + 1
		key := p + ":" + strconv.Itoa(line)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, CallerSite{Path: p, Line: line})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Line < out[j].Line
	})
	return out
}
