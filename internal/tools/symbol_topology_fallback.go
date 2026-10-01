package tools

import (
	"context"
	"math"
	"os"
	"slices"
	"strings"
	"unicode"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/topology"
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

// nodeToDocSymbol converts a topology node to a flat DocumentSymbol. When the
// node carries a byte-precise span (HasBytes), the range is char-precise: the
// real start/end columns the extractor recorded. Otherwise it falls back to a
// line-granular range — start of the first line to the end of the last line's
// content — which matches how an LSP reports a whole declaration closely enough.
func nodeToDocSymbol(n topology.Node, lines []string) protocol.DocumentSymbol {
	var rng protocol.Range
	if n.HasBytes {
		rng = protocol.Range{
			Start: protocol.Position{Line: lineToUint32(n.StartLine - 1), Character: lineToUint32(n.StartCol)},
			End:   protocol.Position{Line: lineToUint32(n.EndLine - 1), Character: lineToUint32(n.EndCol)},
		}
	} else {
		rng = lineGranularRange(n, lines)
	}
	return protocol.DocumentSymbol{
		Name:           n.Name,
		Kind:           topoKindToSymbolKind(n.Kind),
		Range:          rng,
		SelectionRange: rng,
	}
}

// lineGranularRange is the fallback whole-declaration range used when a node has
// no byte-precise span: column 0 of the first line to the end-of-content of the
// last line.
func lineGranularRange(n topology.Node, lines []string) protocol.Range {
	start := lineToUint32(n.StartLine - 1)
	endIdx := max(n.EndLine-1, int(start))
	endChar := 0
	if endIdx >= 0 && endIdx < len(lines) {
		endChar = len(lines[endIdx])
	}
	return protocol.Range{
		Start: protocol.Position{Line: start, Character: 0},
		End:   protocol.Position{Line: lineToUint32(endIdx), Character: lineToUint32(endChar)},
	}
}

// byteOffsetToPosition converts a 0-based byte offset into the file content into
// an LSP Position (0-based line, 0-based byte-column). Returns ok=false when the
// offset is out of range. Columns are byte columns, matching the byte-precise
// spans the extractors record.
func byteOffsetToPosition(content []byte, off int) (pos protocol.Position, ok bool) {
	if off < 0 || off > len(content) {
		return protocol.Position{}, false
	}
	line, lineStart := 0, 0
	for i := range off {
		if content[i] == '\n' {
			line++
			lineStart = i + 1
		}
	}
	return protocol.Position{Line: lineToUint32(line), Character: lineToUint32(off - lineStart)}, true
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

// topologyNodeByPath finds the node a slash-separated name_path (the
// symbol-edit tools' addressing) names, or nil when none does.
//
// A plain name matches the first node of that name, as it always has. A
// multi-segment path names a leaf under a parent, and a leaf whose parent is not
// evidenced is NOT a match: this used to fall back to the first node with the
// leaf's name, which resolved "S/Run" — or any mistyped parent — to some other
// receiver's Run and let move_symbol move it (PR #559 review). A parent is
// evidenced two ways, because extractors differ in whether they qualify a member:
//   - its Qualified carries the parent as a whole identifier segment: the Go
//     extractor's "(*S).Run", the dotted forms of Ruby, Scala and C++;
//   - a node named for the parent encloses it: Python, Java, Rust, Kotlin and
//     others record a member's Qualified as its bare name, so containment is
//     the only evidence they give.
//
// The parent segment has its type parameters stripped, so "S[T]/Run" names S's
// method, matching what the Go extractor records for a generic receiver. An
// empty segment is a malformed path and matches nothing.
func topologyNodeByPath(nodes []topology.Node, namePath string) *topology.Node {
	parts := strings.Split(namePath, "/")
	leaf := parts[len(parts)-1]
	if len(parts) == 1 {
		for i := range nodes {
			if nodes[i].Name == leaf {
				return &nodes[i]
			}
		}
		return nil
	}
	if slices.Contains(parts, "") {
		return nil
	}
	parent := stripTypeParams(parts[len(parts)-2])
	for i := range nodes {
		n := &nodes[i]
		if n.Name == leaf && (qualifiedHasParent(n.Qualified, parent) || enclosedByNamed(nodes, i, parent)) {
			return n
		}
	}
	return nil
}

// enclosedByNamed reports whether another node named parent encloses nodes[i]:
// the evidence of a parent for an extractor that does not qualify its members.
// Byte spans decide when both nodes carry them (two nodes on one line, a
// one-line class, are still ordered), the line range otherwise. A node never
// encloses itself, nor a node with exactly its own span.
func enclosedByNamed(nodes []topology.Node, i int, parent string) bool {
	n := nodes[i]
	for j := range nodes {
		p := nodes[j]
		if j == i || p.Name != parent {
			continue
		}
		if p.HasBytes && n.HasBytes {
			if p.StartByte <= n.StartByte && n.EndByte <= p.EndByte && (p.StartByte != n.StartByte || p.EndByte != n.EndByte) {
				return true
			}
			continue
		}
		if p.StartLine <= n.StartLine && n.EndLine <= p.EndLine && (p.StartLine != n.StartLine || p.EndLine != n.EndLine) {
			return true
		}
	}
	return false
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
	return slices.Contains(qualifiedSegments(qualified), parent)
}

// qualifiedHasParent is qualifiedHasSegment for a parent OTHER than the node
// itself: the last segment of a Qualified is the node's own name, which is no
// evidence of a parent — a bare "run" would otherwise be the parent of "run",
// and "(Run).Run" resolve a "Run/Run" path through its method name alone.
func qualifiedHasParent(qualified, parent string) bool {
	segs := qualifiedSegments(qualified)
	if parent == "" || len(segs) < 2 {
		return false
	}
	return slices.Contains(segs[:len(segs)-1], parent)
}

// qualifiedSegments splits a Qualified name into its identifier segments.
func qualifiedSegments(qualified string) []string {
	return strings.FieldsFunc(qualified, func(r rune) bool {
		return r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}
