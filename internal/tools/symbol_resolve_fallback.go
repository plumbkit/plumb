package tools

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// symbol_resolve_fallback.go is how the symbol-edit tools turn a name_path into
// a symbol: the language server's document-symbol tree first, a fresh tree-sitter
// parse only for a server that did not answer.

// symbolFallbackReason says why the tree-sitter fallback answered instead of
// the language server. The distinction reaches the agent: a server that is
// ABSENT and one that simply did not answer inside its attempt budget call for
// different responses (give up on the LSP vs. retry once it is warm), and both
// hand back a line-granular range rather than a byte-precise one.
type symbolFallbackReason int

const (
	fallbackNotUsed symbolFallbackReason = iota
	fallbackLSPUnavailable
	fallbackLSPTimedOut
)

// symbolTreeResolver finds the symbol namePath names in a document-symbol tree
// the language server ANSWERED. A *symbolNotFoundError says the tree does not
// hold it; any other error is the tool's verdict on the tree it was handed
// (ambiguous, not movable) and is final — the fallback never overrules it.
type symbolTreeResolver func(syms []protocol.DocumentSymbol, uri, namePath string) (*protocol.DocumentSymbol, error)

// resolveSymbolOrFallback resolves namePath via the LSP document-symbol tree,
// falling back to a fresh tree-sitter parse (topology) when the language server
// did not answer. The reason reports which path produced the symbol, and why, so
// the caller can annotate its output (the fallback range is line-granular, not
// byte-precise). When the LSP fails and no fallback resolves the symbol, the
// original LSP error is returned.
//
// It takes TWO contexts on purpose. lspCtx bounds the server attempt and is
// spent once that attempt misses its budget; ctx is the caller's live context
// and is what the fallback runs on. Handing the fallback lspCtx — which is what
// every symbol-edit tool used to do — makes it inoperative rather than merely
// late: topology's safeExtract refuses to start a parse on an expired context,
// so the tool surfaces the very timeout the fallback exists to replace
// (PLAN-390, PLAN-403). See withFallbackLSPDeadline.
func resolveSymbolOrFallback(ctx, lspCtx context.Context, client lsp.Client, topo topologyStoreFn, warmup LSPWarmupFn, uri, namePath string) (sym *protocol.DocumentSymbol, reason symbolFallbackReason, err error) {
	return resolveSymbolWith(ctx, lspCtx, client, topo, warmup, uri, namePath, findInSymbolTree)
}

// resolveSymbolWith is resolveSymbolOrFallback with the tool's own rule for
// reading the server's tree. The fallback is only ever the answer of a server
// that gave none: one that errored or timed out, or one still warming, whose
// tree — an empty one, from sourcekit-lsp and jdtls before they finish indexing
// — says nothing yet about what the file holds. A server that is ready and
// answered is not unavailable, and "that name is not in this file" is its
// answer. Asking the index behind it picked a symbol the server never named and
// then blamed the server in the banner (#571).
func resolveSymbolWith(ctx, lspCtx context.Context, client lsp.Client, topo topologyStoreFn, warmup LSPWarmupFn, uri, namePath string, resolve symbolTreeResolver) (*protocol.DocumentSymbol, symbolFallbackReason, error) {
	syms, lspErr := documentSymbolTree(lspCtx, client, uri)
	if lspErr == nil {
		sym, err := resolve(syms, uri, namePath)
		if err == nil {
			return sym, fallbackNotUsed, nil
		}
		if !coldServerMiss(err, warmup, uri) {
			return nil, fallbackNotUsed, err
		}
		lspErr = err
	}
	if IsWorkspaceBoundaryError(lspErr) {
		return nil, fallbackNotUsed, lspErr
	}
	return resolveByTopology(ctx, lspCtx, topo, uri, namePath, lspErr)
}

// coldServerMiss reports whether err is a "not in this tree" answer from a
// server that is still warming, which is no evidence about the file.
func coldServerMiss(err error, warmup LSPWarmupFn, uri string) bool {
	var notFound *symbolNotFoundError
	if !errors.As(err, &notFound) {
		return false
	}
	warming, _ := lspWarmup(warmup, uri)
	return warming
}

// resolveByTopology resolves namePath from a fresh tree-sitter parse of uri.
// lspErr is why the server did not settle it, and is what the caller gets back
// when the index cannot either.
func resolveByTopology(ctx, lspCtx context.Context, topo topologyStoreFn, uri, namePath string, lspErr error) (*protocol.DocumentSymbol, symbolFallbackReason, error) {
	nodes, edges, ok := freshTopologyGraph(ctx, topo, uri)
	if !ok {
		return nil, fallbackNotUsed, lspErr
	}
	matches := topologyNodesByPath(nodes, edges, namePath)
	if len(matches) > 1 {
		return nil, fallbackNotUsed, topologyAmbiguityErr(lspErr, namePath, matches)
	}
	if len(matches) == 0 {
		// Both trees were asked and neither has the path. When the server itself
		// answered "not found" that is already the message; when it failed to
		// answer, its error alone ("did not respond in time — retry shortly")
		// would send the agent to retry a path that no retry can resolve.
		var notFound *symbolNotFoundError
		if errors.As(lspErr, &notFound) {
			return nil, fallbackNotUsed, lspErr
		}
		return nil, fallbackNotUsed, fmt.Errorf("%w; the tree-sitter fallback finds no symbol %q in %s either",
			lspErr, namePath, paths.URIToPath(uri))
	}
	ds := nodeToDocSymbol(*matches[0], fileLines(paths.URIToPath(uri)))
	return &ds, lspFallbackReason(lspCtx), nil
}

// lspFallbackReason classifies a failed server attempt from the attempt context
// itself, so the error text the LSP path returns stays untouched: an expired
// lspCtx means the server was too slow, anything else means it could not answer
// at all.
func lspFallbackReason(lspCtx context.Context) symbolFallbackReason {
	if errors.Is(lspCtx.Err(), context.DeadlineExceeded) {
		return fallbackLSPTimedOut
	}
	return fallbackLSPUnavailable
}

// resolveSymbol fetches the DocumentSymbol tree for uri and locates namePath.
func resolveSymbol(ctx context.Context, client lsp.Client, uri, namePath string) (*protocol.DocumentSymbol, error) {
	syms, err := documentSymbolTree(ctx, client, uri)
	if err != nil {
		return nil, err
	}
	return findInSymbolTree(syms, uri, namePath)
}

// documentSymbolTree asks the server for uri's symbol tree, rewording a missed
// deadline as the retry-shortly advice an agent can act on.
func documentSymbolTree(ctx context.Context, client lsp.Client, uri string) ([]protocol.DocumentSymbol, error) {
	syms, err := client.DocumentSymbols(ctx, protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, errors.New("language server did not respond in time (it may still be indexing the workspace — retry shortly)")
		}
		return nil, fmt.Errorf("documentSymbols: %w", err)
	}
	return syms, nil
}

// findInSymbolTree is the symbol-edit tools' symbolTreeResolver: namePath
// followed from the top of the tree.
func findInSymbolTree(syms []protocol.DocumentSymbol, uri, namePath string) (*protocol.DocumentSymbol, error) {
	if sym := findSymbolByPath(syms, namePath); sym != nil {
		return sym, nil
	}
	return nil, &symbolNotFoundError{namePath: namePath, path: paths.URIToPath(uri), nested: nestedNamePaths(syms, namePath)}
}

// nestedNamePaths lists the name_paths of nested symbols a plain namePath
// matches, each proven to resolve (moveNamePaths): a plain name addresses a
// top-level symbol, so a member of that name is found only by its full path.
// nil for a path, and when any match has no path that resolves.
func nestedNamePaths(syms []protocol.DocumentSymbol, namePath string) []string {
	if strings.Contains(namePath, "/") {
		return nil
	}
	return moveNamePaths(syms, resolveSymbolsByName(syms, namePath))
}

// symbolNotFoundError is the language server's answer that namePath is not in
// the file, as opposed to its failure to answer. resolveSymbolOrFallback tells
// the two apart: only a failed server leaves the fallback's own miss worth
// saying aloud.
type symbolNotFoundError struct {
	namePath, path string
	nested         []string // name_paths of same-named nested symbols that do resolve
}

func (e *symbolNotFoundError) Error() string {
	msg := fmt.Sprintf("symbol %q not found in %s", e.namePath, e.path)
	if len(e.nested) == 0 {
		return msg
	}
	quoted := make([]string, len(e.nested))
	for i, p := range e.nested {
		quoted[i] = strconv.Quote(p)
	}
	return msg + " as a top-level symbol; a plain name does not reach a nested one — pass its full name_path: " + strings.Join(quoted, ", ")
}
