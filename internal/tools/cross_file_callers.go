package tools

import (
	"context"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
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
type CrossFileCallersFunc func(ctx context.Context, path, name string, startLine, endLine int) []CallerSite

// NewLSPCrossFileCallers builds a CrossFileCallersFunc backed by the language
// server. It resolves the symbol's identifier position via the DocumentSymbol
// SelectionRange (the same position get_definition/find_references query),
// requests its references, and keeps those outside the symbol's own file.
// Returns nil when client is nil.
//
// workspaceFn supplies the current workspace root (evaluated per call, so a
// re-pinned connection is handled). It absolutises the incoming path: topology
// node paths are workspace-relative, but the language server needs an absolute
// file:// URI, so a relative path is joined onto the root before the query.
func NewLSPCrossFileCallers(client lsp.Client, c *cache.Cache, ttl, timeout time.Duration, workspaceFn func() string) CrossFileCallersFunc {
	if client == nil {
		return nil
	}
	return func(ctx context.Context, path, name string, startLine, endLine int) []CallerSite {
		if name == "" || path == "" {
			return nil
		}
		root := ""
		if workspaceFn != nil {
			root = workspaceFn()
		}
		if !filepath.IsAbs(path) && root != "" {
			path = filepath.Join(root, path)
		}
		uri := toFileURI(path)

		ctx, cancel := withLSPDeadline(ctx, timeout)
		defer cancel()

		syms := cachedDocumentSymbols(ctx, client, c, ttl, uri)
		target, ok := crossFileTarget(syms, name, startLine, endLine)
		if !ok {
			return nil
		}

		locs, err := client.References(ctx, protocol.ReferenceParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri},
			Position:     target.SelectionRange.Start,
			Context:      protocol.ReferenceContext{IncludeDeclaration: false},
		})
		if err != nil {
			return nil
		}
		return crossFileSites(locs, uri, root)
	}
}

// crossFileTarget picks the document symbol a topology node names. Topology
// passes a Go method's bare name, which several receivers in one file share
// (Close, String, Execute), so the node's 1-based span [startLine, endLine]
// chooses among every symbol carrying the name, method or not: two declarations
// never overlap. Without a span, or when the span picks out no single one (a
// stale index), only a name exactly one symbol carries resolves — reporting the
// first match's callers would attribute them to the wrong symbol.
func crossFileTarget(syms []protocol.DocumentSymbol, name string, startLine, endLine int) (protocol.DocumentSymbol, bool) {
	if strings.Contains(name, ".") {
		if matches := resolveSymbolsByName(syms, name); len(matches) == 1 {
			return matches[0], true
		}
		return protocol.DocumentSymbol{}, false
	}
	exact, methods := plainNameCandidates(syms, name)
	cands := slices.Concat(exact, methods)
	if startLine > 0 {
		if endLine < startLine {
			endLine = startLine
		}
		from, to := uint32(startLine-1), uint32(endLine-1) //nolint:gosec // both positive, checked above
		var hit []protocol.DocumentSymbol
		for _, s := range cands {
			if s.Range.Start.Line <= to && s.Range.End.Line >= from {
				hit = append(hit, s)
			}
		}
		if len(hit) == 1 {
			return hit[0], true
		}
	}
	if len(cands) == 1 {
		return cands[0], true
	}
	return protocol.DocumentSymbol{}, false
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
// selfURI, ordered by path then line for deterministic output. Paths are made
// workspace-relative when they fall under workspaceRoot, so the block matches
// topology's relative-path style; paths outside the root stay absolute.
func crossFileSites(locs []protocol.Location, selfURI, workspaceRoot string) []CallerSite {
	self := paths.URIToPath(selfURI)
	seen := map[string]bool{}
	var out []CallerSite
	for _, l := range locs {
		p := paths.URIToPath(l.URI)
		if p == self {
			continue // intra-file: the topology call graph already covers it
		}
		p = relativeToRoot(p, workspaceRoot)
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

// relativeToRoot returns p relative to root when p is inside it, else p
// unchanged. Keeps the cross-file caller block consistent with topology's
// workspace-relative paths.
func relativeToRoot(p, root string) string {
	if root == "" {
		return p
	}
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return rel
}
