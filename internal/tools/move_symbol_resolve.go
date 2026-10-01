package tools

// move_symbol_resolve.go — how move_symbol picks the declaration to move, split
// from move_symbol.go for its size cap.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// resolveMoveTarget locates name_path in the source, refusing an ambiguous bare
// name (two declarations share it — moving "the first" would be a silent guess)
// and a symbol that is not a top-level declaration. The language server's tree
// decides both whenever the server answers, and the shared resolver turns to
// tree-sitter only for a server that did not (resolveSymbolWith), so a move the
// resolver named is never carried out on another symbol.
//
// The ambiguity refusal is asked of WHICHEVER TREE ANSWERED. Gating it on the
// language server alone made it fire exactly when it was least needed (healthy
// server) and skipped it exactly where the tool is least sure of itself: a cold
// or slow server leaves the answer to a line-granular tree-sitter parse, and
// topologyNodesByPath answers a plain name with the FIRST node of that name, so
// the move proceeded on a silent guess and rewrote two files (PLAN-403 review §1).
func (t *MoveSymbol) resolveMoveTarget(ctx, lspCtx context.Context, uri, namePath string) (*protocol.DocumentSymbol, symbolFallbackReason, error) {
	sym, reason, err := resolveSymbolWith(ctx, lspCtx, t.client, t.topo, t.warmup, uri, namePath, resolveMoveInTree)
	if err != nil {
		return nil, fallbackNotUsed, fmt.Errorf("move_symbol: %w", err)
	}
	// Only when tree-sitter answered: the server's tree never settled the name
	// (it errored, timed out or was still warming), so the ambiguity is asked of
	// the index instead. A warm resolve keeps the server's own verdict —
	// re-asking topology there would refuse moves gopls considers unambiguous.
	if reason != fallbackNotUsed && !strings.Contains(namePath, "/") {
		if nodes, ok := freshTopologyNodes(ctx, t.topo, uri); ok {
			if n := len(topologyNodesByName(nodes, namePath)); n > 1 {
				return nil, fallbackNotUsed, fmt.Errorf("move_symbol: %w", moveAmbiguousErr(n, namePath, uri, nil))
			}
		}
	}
	return sym, reason, nil
}

// resolveMoveInTree is move_symbol's symbolTreeResolver. A bare name goes
// through the resolver every other tool shares, so the declaration it names is
// the one moved — a method gopls lists as "(*S).Run" included, which a walk of
// the tree from the top never matched by its plain name — while a slash path is
// followed from the top.
//
// A bare name selects top-level declarations. When the one symbol the resolver
// matches is a member of a type or a body (a struct field, an interface method,
// a class member), the caller did not say it meant a member, and the move is
// refused rather than carried out on it or on a different symbol the index
// likes better (#571). A slash path is the caller naming the nesting itself, and
// is honoured.
func resolveMoveInTree(syms []protocol.DocumentSymbol, uri, namePath string) (*protocol.DocumentSymbol, error) {
	sym, err := moveCandidate(syms, uri, namePath)
	if err != nil || strings.Contains(namePath, "/") {
		return sym, err
	}
	if chain, found := enclosingSymbols(syms, *sym); found && hasMemberParent(chain) {
		return nil, moveNestedErr(syms, uri, namePath, *sym, chain)
	}
	return sym, nil
}

// moveCandidate is the one symbol namePath names in the server's tree, before
// the top-level check: a *symbolNotFoundError when it names none, the ambiguity
// refusal when it names several.
func moveCandidate(syms []protocol.DocumentSymbol, uri, namePath string) (*protocol.DocumentSymbol, error) {
	notFound := &symbolNotFoundError{namePath: namePath, path: paths.URIToPath(uri)}
	if strings.Contains(namePath, "/") {
		if sym := findSymbolByPath(syms, namePath); sym != nil {
			return sym, nil
		}
		return nil, notFound
	}
	switch m := resolveSymbolsByName(syms, namePath); len(m) {
	case 0:
		return nil, notFound
	case 1:
		return &m[0], nil
	default:
		return nil, moveAmbiguousErr(len(m), namePath, uri, moveNamePaths(syms, m))
	}
}

// enclosingSymbols returns target's ancestors in the tree, outermost first, and
// whether target is in the tree at all.
func enclosingSymbols(syms []protocol.DocumentSymbol, target protocol.DocumentSymbol) ([]protocol.DocumentSymbol, bool) {
	for _, s := range syms {
		if sameSymbol(s, target) {
			return nil, true
		}
		if rest, found := enclosingSymbols(s.Children, target); found {
			return append([]protocol.DocumentSymbol{s}, rest...), true
		}
	}
	return nil, false
}

// hasMemberParent reports whether any ancestor is a type or a body — something
// a member is cut out of — rather than a grouping the server draws around
// declarations that are top-level in the language: a namespace, a module, a
// package, a file.
func hasMemberParent(chain []protocol.DocumentSymbol) bool {
	for _, a := range chain {
		switch a.Kind {
		case protocol.SKFile, protocol.SKModule, protocol.SKNamespace, protocol.SKPackage:
		default:
			return true
		}
	}
	return false
}

// moveNestedErr refuses a plain name whose one match is a member of another
// symbol. It names the symbol by its full path and says what it is. When the
// plain name has methods too (a struct field T.Run hides every (*S).Run from a
// plain Run, and the field is what the refusal is about) it offers their paths,
// since the method is what the agent most likely meant.
func moveNestedErr(syms []protocol.DocumentSymbol, uri, namePath string, sym protocol.DocumentSymbol, chain []protocol.DocumentSymbol) error {
	names := make([]string, 0, len(chain)+1)
	for _, a := range chain {
		names = append(names, a.Name)
	}
	parent := strings.Join(names, "/")
	kind := strings.ToLower(symbolKindNames[sym.Kind])
	if kind == "" {
		kind = "member"
	}
	msg := fmt.Sprintf("%q in %s resolves to %s/%s, a %s nested in %s — a plain name moves top-level declarations only; "+
		"selecting this one takes its full name_path, %q", namePath, paths.URIToPath(uri), parent, sym.Name, kind, parent, parent+"/"+sym.Name)
	if hint := moveMethodHint(syms, namePath); hint != "" {
		msg += "; " + hint
	}
	return errors.New(msg)
}

// moveMethodHint offers the name_path of each method a plain namePath names
// once the symbols carrying the name itself are set aside, each proven to
// resolve; "" when there is none, or when a path could not be proven.
func moveMethodHint(syms []protocol.DocumentSymbol, namePath string) string {
	if strings.ContainsAny(namePath, "/.") {
		return ""
	}
	_, methods := plainNameCandidates(syms, namePath)
	namePaths := moveNamePaths(syms, methods)
	if len(namePaths) == 0 {
		return ""
	}
	quoted := make([]string, len(namePaths))
	for i, p := range namePaths {
		quoted[i] = strconv.Quote(p)
	}
	return "to move the method of that name, pass name_path " + strings.Join(quoted, " or ")
}
