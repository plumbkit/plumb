package treesitter

import (
	"math"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"

	"github.com/plumbkit/plumb/internal/topology"
)

// span returns the byte-precise declaration span (0-based byte offsets) and the
// 0-based start/end columns of n, ready to assign onto a topology.Node. The
// gotreesitter Point columns are already 0-based, matching topology.Node's
// convention. Byte/column values are clamped into int range.
func span(n *tsg.Node) (startByte, endByte, startCol, endCol int) {
	return clampU32(n.StartByte()), clampU32(n.EndByte()),
		clampU32(n.StartPoint().Column), clampU32(n.EndPoint().Column)
}

// setSpan stamps the byte-precise declaration span of tn onto node, marking it
// HasBytes. It is the single seam the gotreesitter extractors call so every
// emitted node carries its exact span. The optional doc-comment span is set
// separately (via docSpanBefore) only by extractors with a reliable doc node.
func setSpan(node *topology.Node, tn *tsg.Node) {
	node.HasBytes = true
	node.StartByte, node.EndByte, node.StartCol, node.EndCol = span(tn)
}

// docSpanBefore returns the byte span of a contiguous comment block immediately
// preceding decl (its previous siblings of a comment type, with no intervening
// non-comment node). Returns (0, 0) — the "no doc span" sentinel — when there is
// no such block. isComment reports whether a node type is a comment in the
// grammar (it varies: "comment", "line_comment", "block_comment", …).
func docSpanBefore(decl *tsg.Node, lang *tsg.Language, isComment func(typ string) bool) (start, end int) {
	var first *tsg.Node
	for sib := decl.PrevSibling(); sib != nil; sib = sib.PrevSibling() {
		if !isComment(sib.Type(lang)) {
			break
		}
		first = sib
	}
	if first == nil {
		return 0, 0
	}
	last := decl.PrevSibling() // the comment closest to the declaration
	return clampU32(first.StartByte()), clampU32(last.EndByte())
}

// clampU32 narrows a tree-sitter uint32 offset/column into int range.
func clampU32(v uint32) int {
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(v)
}

// firstNamedChild returns the first named child of n, or nil when n has none.
func firstNamedChild(n *tsg.Node) *tsg.Node {
	for _, c := range n.Children() {
		if c.IsNamed() {
			return c
		}
	}
	return nil
}

// childByType returns the first child of n whose node type is typ, or nil.
func childByType(n *tsg.Node, typ string, lang *tsg.Language) *tsg.Node {
	for _, c := range n.Children() {
		if c.Type(lang) == typ {
			return c
		}
	}
	return nil
}

// lastSegment returns the final segment of a "::"-separated path, so
// "tokio::test" → "test" and a bare "test" is returned unchanged.
func lastSegment(path string) string {
	if i := strings.LastIndex(path, "::"); i >= 0 {
		return path[i+2:]
	}
	return path
}

// callableNameCounts counts how many function / method nodes in nodes share
// each Name. The tree-sitter extractors resolve call edges by callee name alone
// — they carry no type information — and funcIdx keeps only one index per name,
// so this is the only way to tell that a name is shared. Built once per file,
// after every node has been collected.
//
// KindTest is deliberately excluded: in describe/it/test-style suites a test
// node's Name is its description string (e.g. describe('greet', …)), not a
// callable identifier, so counting it would falsely mark a real same-named
// function as ambiguous.
func callableNameCounts(nodes []topology.Node) map[string]int {
	counts := make(map[string]int)
	for i := range nodes {
		switch nodes[i].Kind {
		case topology.KindFunction, topology.KindMethod:
			counts[nodes[i].Name]++
		}
	}
	return counts
}

// heuristicCallEdge builds a name-resolved EdgeCalls from → to. The call site is
// syntactically certain but the callee is resolved by name alone (no type
// information), so it is a 0.8 heuristic. When more than one callable shares the
// target's name in the same file the match is ambiguous — a receiver-blind
// `recv.name()` could mean any of them — so the edge is down-weighted to 0.5 and
// flagged source="heuristic-ambiguous" rather than asserting a confident edge to
// an arbitrary same-named target. See issue #30. nameCounts comes from
// callableNameCounts; nodes lets the target name be recovered from its index.
func heuristicCallEdge(from, to int64, nodes []topology.Node, nameCounts map[string]int) topology.Edge {
	e := topology.Edge{
		FromID:     from,
		ToID:       to,
		Kind:       topology.EdgeCalls,
		Confidence: 0.8,
		Source:     "heuristic",
	}
	if to >= 0 && int(to) < len(nodes) && nameCounts[nodes[to].Name] > 1 {
		e.Confidence = 0.5
		e.Source = "heuristic-ambiguous"
	}
	return e
}
