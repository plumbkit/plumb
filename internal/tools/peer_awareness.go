package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/plumbkit/plumb/internal/topology"
)

// peer_awareness.go holds the shared, best-effort helpers behind the tier-1
// cross-agent peer-awareness signals ([collab] peer_awareness): the exported
// write-tool list and tool-input path extractor (reused by the connection-side
// peer-hint path), plus the topology annotation that turns a bare peer write
// path into "package tools · RateLimiter, Allow". Everything here is advisory
// and derived from writes the daemon itself performed or watched.

// WriteToolNames returns a copy of the mutating MCP tool names used by the
// recent-writes feed, so the connection-side peer-hint path can query the same
// set without duplicating the list.
func WriteToolNames() []string {
	return append([]string(nil), writeToolNames...)
}

// FileFromToolInput extracts the first workspace file path from a mutating tool
// call's raw input JSON (file_path / from / path, or a transaction's first op).
// Returns "" when no single path applies (a git or find_replace call). Exported
// wrapper over fileFromInputJSON for reuse outside this package.
func FileFromToolInput(raw string) string {
	return fileFromInputJSON(raw)
}

// maxAnnotatedSymbols caps how many symbol names a topology annotation lists
// before collapsing the remainder to "(+N more)", keeping the line compact.
const maxAnnotatedSymbols = 3

// fileTopologyAnnotation resolves a compact, best-effort package/symbol label
// for a file from the topology index, e.g. "package tools · RateLimiter, Allow
// (+2 more)". A peer write records only a path (no line), so the annotation is
// file-level: the enclosing package plus the file's leading declared symbols.
// Returns "" when the store is nil, the file is not indexed, or the query errors
// — the caller then shows the bare path (the shipped behaviour). Source is the
// topology index and therefore approximate.
func fileTopologyAnnotation(ctx context.Context, store *topology.Store, absPath string) string {
	if store == nil {
		return ""
	}
	nodes, err := store.SymbolsInFile(ctx, absPath)
	if err != nil || len(nodes) == 0 {
		return ""
	}
	var pkg string
	var syms []string
	for _, n := range nodes {
		switch n.Kind {
		case topology.KindPackage:
			if pkg == "" {
				pkg = n.Name
			}
		case topology.KindFunction, topology.KindMethod, topology.KindType,
			topology.KindClass, topology.KindConstant:
			if n.Name != "" {
				syms = append(syms, n.Name)
			}
		}
	}
	return joinTopologyAnnotation(pkg, syms)
}

// joinTopologyAnnotation renders the package + symbol summary. Pure helper.
func joinTopologyAnnotation(pkg string, syms []string) string {
	extra := 0
	if len(syms) > maxAnnotatedSymbols {
		extra = len(syms) - maxAnnotatedSymbols
		syms = syms[:maxAnnotatedSymbols]
	}
	var symPart string
	if len(syms) > 0 {
		symPart = strings.Join(syms, ", ")
		if extra > 0 {
			symPart += fmt.Sprintf(" (+%d more)", extra)
		}
	}
	switch {
	case pkg != "" && symPart != "":
		return fmt.Sprintf("package %s · %s", pkg, symPart)
	case pkg != "":
		return "package " + pkg
	default:
		return symPart
	}
}
