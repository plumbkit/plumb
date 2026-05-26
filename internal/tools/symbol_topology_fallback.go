package tools

import (
	"context"
	"math"
	"os"
	"strings"
	"unicode"

	"github.com/golimpio/plumb/internal/lsp/protocol"
	"github.com/golimpio/plumb/internal/topology"
)

// symbol_topology_fallback.go provides the tree-sitter (topology) fallback for
// the symbol-oriented tools (read_symbol and the non-deleting symbol-edit
// tools) so they keep working when the language server is cold, absent, or
// cannot parse the file.
//
// The fallback re-parses the CURRENT file content (Store.ExtractFile — not the
// possibly-stale persisted index) so the resolved ranges reflect the file
// exactly as it is on disk. Ranges are line-granular: start of the symbol's
// first line to the end of its last line's content, which matches how an LSP
// reports a whole declaration closely enough for read / replace / insert.
// safe_delete_symbol deliberately has NO fallback: its safety guarantee is the
// LSP reference check, which topology cannot reproduce.

// topoKindToSymbolKind maps a topology node kind to the nearest LSP SymbolKind
// for display in fallback output.
func topoKindToSymbolKind(k topology.NodeKind) protocol.SymbolKind {
	switch k {
	case topology.KindFunction, topology.KindTest:
		return protocol.SKFunction
	case topology.KindMethod:
		return protocol.SKMethod
	case topology.KindClass:
		return protocol.SKClass
	case topology.KindType:
		return protocol.SKStruct
	case topology.KindConstant:
		return protocol.SKConstant
	case topology.KindVariable:
		return protocol.SKVariable
	case topology.KindField:
		return protocol.SKField
	case topology.KindImport, topology.KindPackage:
		return protocol.SKModule
	case topology.KindSection:
		return protocol.SKNamespace
	default:
		return protocol.SKVariable
	}
}

// freshTopologyNodes re-parses uri's current content via the topology store and
// returns its nodes. ok is false when topology is unavailable or no extractor
// handles the file, so the caller surfaces the original LSP error.
func freshTopologyNodes(ctx context.Context, fn topologyStoreFn, uri string) (nodes []topology.Node, ok bool) {
	store := activeTopology(fn)
	if store == nil {
		return nil, false
	}
	nodes, err := store.ExtractFile(ctx, uri)
	if err != nil || len(nodes) == 0 {
		return nil, false
	}
	return nodes, true
}

// nodeToDocSymbol converts a topology node to a flat DocumentSymbol with a
// line-granular range computed from the file's lines: start of the first line
// to the end of the last line's content.
func nodeToDocSymbol(n topology.Node, lines []string) protocol.DocumentSymbol {
	start := lineToUint32(n.StartLine - 1)
	endIdx := n.EndLine - 1
	if endIdx < int(start) {
		endIdx = int(start)
	}
	endChar := 0
	if endIdx >= 0 && endIdx < len(lines) {
		endChar = len(lines[endIdx])
	}
	rng := protocol.Range{
		Start: protocol.Position{Line: start, Character: 0},
		End:   protocol.Position{Line: lineToUint32(endIdx), Character: lineToUint32(endChar)},
	}
	return protocol.DocumentSymbol{
		Name:           n.Name,
		Kind:           topoKindToSymbolKind(n.Kind),
		Range:          rng,
		SelectionRange: rng,
	}
}

// lineToUint32 clamps a line/column number into the uint32 range LSP positions
// use, guarding against negatives and overflow.
func lineToUint32(v int) uint32 {
	if v < 0 {
		return 0
	}
	if v > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(v) //nolint:gosec // G115: bounds-checked immediately above
}

// fileLines reads path and splits it into lines, or returns nil on error.
func fileLines(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Split(string(data), "\n")
}

// topologyNodesByName returns the nodes matching name, used by read_symbol's
// fallback. A dotted "ReceiverType.MethodName" matches the leaf node whose
// Qualified name contains the receiver; a plain name matches every node with
// that name.
func topologyNodesByName(nodes []topology.Node, name string) []topology.Node {
	if parent, child, ok := strings.Cut(name, "."); ok {
		var out []topology.Node
		for _, n := range nodes {
			if n.Name == child && qualifiedHasSegment(n.Qualified, parent) {
				out = append(out, n)
			}
		}
		return out
	}
	var out []topology.Node
	for _, n := range nodes {
		if n.Name == name {
			out = append(out, n)
		}
	}
	return out
}

// topologyNodeByPath finds the single node matching a slash-separated name_path
// (the symbol-edit tools' addressing). It matches the leaf segment by name and,
// when a parent segment is present, prefers a node whose Qualified contains it.
func topologyNodeByPath(nodes []topology.Node, namePath string) *topology.Node {
	parts := strings.Split(namePath, "/")
	leaf := parts[len(parts)-1]
	parent := ""
	if len(parts) > 1 {
		parent = parts[len(parts)-2]
	}
	var fallback *topology.Node
	for i := range nodes {
		n := &nodes[i]
		if n.Name != leaf {
			continue
		}
		if parent == "" || qualifiedHasSegment(n.Qualified, parent) {
			return n
		}
		if fallback == nil {
			fallback = n
		}
	}
	return fallback
}

// qualifiedHasSegment reports whether parent appears as a whole identifier
// segment of qualified, where segments are the maximal runs of identifier runes
// (letters, digits, underscore). This matches a receiver/parent name without the
// substring false positives strings.Contains would allow: "User" matches
// "(User).Save" and "(*User).Save" but NOT "SuperUser.Save".
func qualifiedHasSegment(qualified, parent string) bool {
	if parent == "" {
		return false
	}
	segs := strings.FieldsFunc(qualified, func(r rune) bool {
		return r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, seg := range segs {
		if seg == parent {
			return true
		}
	}
	return false
}
