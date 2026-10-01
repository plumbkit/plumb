package tools

import (
	"context"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
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
	nodes, _, ok = freshTopologyGraph(ctx, fn, uri)
	return nodes, ok
}

// freshTopologyGraph is freshTopologyNodes that also returns the edges the
// extractor drew, as indices into nodes: a name_path resolves its parent through
// them where no span says whose member a node is (topologyNodesByPath).
func freshTopologyGraph(ctx context.Context, fn topologyStoreFn, uri string) (nodes []topology.Node, edges []topology.Edge, ok bool) {
	store := activeTopology(fn)
	if store == nil {
		return nil, nil, false
	}
	nodes, edges, err := store.ExtractFileGraph(ctx, uri)
	if err != nil || len(nodes) == 0 {
		return nil, nil, false
	}
	return nodes, edges, true
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

// topologyNodesByPath returns every node a slash-separated name_path (the
// symbol-edit tools' addressing) names: none when nothing does, several when the
// path is ambiguous. Callers refuse both — a write that names one symbol must
// never land on another.
//
// A plain name matches the first node of that name, as it always has. A
// multi-segment path P1/…/Pn/Name names a node called Name whose DIRECT parent is
// Pn, whose own direct parent is Pn-1, and so on up to P1 — the direct-child
// rule findSymbolRecursive applies to the language server's tree, not a node that
// merely sits somewhere inside Pn. (The rule it replaces took any enclosing node
// called Pn and looked at no other segment: Outer/run resolved to the run of a
// class nested in Outer, and Wrong/Inner/run resolved although there is no Wrong
// — PR #559 review.) A node's direct parent is the smallest named node whose span
// encloses it, or the node an extractor's own containment edge ties it to: the
// edge is the only link where the language has no enclosing node of its own, as
// with a Rust method inside `impl Foo`, which the extractor links to the type Foo
// when the file declares it. A Qualified name that spells out the whole chain,
// leaf included, is evidence too — Go's "(*S).Run" is S/Run, C++'s "Foo::run" is
// Foo/run — and nothing short of the whole chain is.
//
// A parent segment has its type parameters and Go receiver decoration stripped,
// as findFlatGoMethod strips a receiver: "S[T]/Run", "*S/Run" and "(*S)/Run" all
// name S's method. An empty segment is a malformed path and matches nothing. A
// language whose extractor records neither a parent node, a containment edge nor
// a qualified name for a member — a TypeScript namespace is not a node — leaves
// that member unaddressable by a path through it: it is refused, not guessed.
func topologyNodesByPath(nodes []topology.Node, edges []topology.Edge, namePath string) []*topology.Node {
	parts := strings.Split(namePath, "/")
	leaf := parts[len(parts)-1]
	if len(parts) == 1 {
		for i := range nodes {
			if nodes[i].Name == leaf {
				return []*topology.Node{&nodes[i]}
			}
		}
		return nil
	}
	if slices.Contains(parts, "") {
		return nil
	}
	chain := make([]string, len(parts)-1)
	for i, p := range parts[:len(parts)-1] {
		if chain[i] = stripTypeParams(goReceiverType(p)); chain[i] == "" {
			return nil
		}
	}
	whole := append(slices.Clone(chain), leaf)
	parents := newTopologyParents(nodes, edges)
	var out []*topology.Node
	for i := range nodes {
		if nodes[i].Name == leaf && (slices.Equal(qualifiedSegments(nodes[i].Qualified), whole) || parents.chainMatches(i, chain)) {
			out = append(out, &nodes[i])
		}
	}
	return out
}

// topologyParents answers "whose direct member is nodes[i]?" from the two
// evidences an extractor leaves: the span that encloses a node, and the
// containment edge that ties it to a type its span does not sit inside.
type topologyParents struct {
	nodes       []topology.Node
	edgeParents map[int][]int // node index -> sources of the containment edges into it
}

func newTopologyParents(nodes []topology.Node, edges []topology.Edge) *topologyParents {
	g := &topologyParents{nodes: nodes, edgeParents: map[int][]int{}}
	for _, e := range edges {
		if e.Kind != topology.EdgeContains || e.FromID == e.ToID ||
			e.FromID < 0 || e.ToID < 0 || e.FromID >= int64(len(nodes)) || e.ToID >= int64(len(nodes)) {
			continue
		}
		from, to := int(e.FromID), int(e.ToID)
		// A package node's containment edge is no evidence of a parent: the
		// package is the file's own scope (Go, PHP and a file-scoped C# namespace
		// link every top-level declaration to it), and gopls' symbols for the same
		// file are flat. A package whose span encloses a declaration, an Elixir
		// module, is a parent through spanEncloses like any other node.
		if canBeParent(nodes[from]) && nodes[from].Kind != topology.KindPackage {
			g.edgeParents[to] = append(g.edgeParents[to], from)
		}
	}
	return g
}

// chainMatches reports whether chain names nodes[i]'s ancestry, nearest last:
// its direct parent is called chain[len-1], that node's direct parent
// chain[len-2], and so on. An empty chain is always met.
func (g *topologyParents) chainMatches(i int, chain []string) bool {
	if len(chain) == 0 {
		return true
	}
	want := chain[len(chain)-1]
	for _, p := range g.of(i) {
		if g.nodes[p].Name == want && g.chainMatches(p, chain[:len(chain)-1]) {
			return true
		}
	}
	return false
}

// of returns the direct parents of nodes[i]: every containment-edge source, and
// the innermost enclosing named nodes — one unless two enclose it with the same
// span, which neither of them is inside.
func (g *topologyParents) of(i int) []int {
	out := slices.Clone(g.edgeParents[i])
	var enclosing []int
	for j := range g.nodes {
		if j != i && canBeParent(g.nodes[j]) && spanEncloses(g.nodes[j], g.nodes[i]) {
			enclosing = append(enclosing, j)
		}
	}
	for _, a := range enclosing {
		innermost := !slices.ContainsFunc(enclosing, func(b int) bool { return a != b && spanEncloses(g.nodes[a], g.nodes[b]) })
		if innermost && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// canBeParent reports whether n is a node a name_path can name as a parent: it
// has a name, and it is not an import (whose "name" is the imported module).
func canBeParent(n topology.Node) bool {
	return n.Name != "" && n.Kind != topology.KindImport
}

// spanEncloses reports whether p's span strictly encloses n's. Byte spans decide
// when both nodes carry them (two nodes on one line, a one-line class, are still
// ordered), the line range otherwise. A node never encloses itself, nor a node
// with exactly its own span.
func spanEncloses(p, n topology.Node) bool {
	if p.HasBytes && n.HasBytes {
		return p.StartByte <= n.StartByte && n.EndByte <= p.EndByte && (p.StartByte != n.StartByte || p.EndByte != n.EndByte)
	}
	return p.StartLine <= n.StartLine && n.EndLine <= p.EndLine && (p.StartLine != n.StartLine || p.EndLine != n.EndLine)
}

// topologyAmbiguityErr says why a name_path the fallback could not choose a
// declaration for was refused: several matched, so none is named. lspErr is why
// the language server did not settle it; the refusal reads after that.
func topologyAmbiguityErr(lspErr error, namePath string, matches []*topology.Node) error {
	lines := make([]int, 0, len(matches))
	for _, m := range matches {
		lines = append(lines, m.StartLine)
	}
	slices.Sort(lines)
	text := make([]string, 0, len(lines))
	for _, l := range slices.Compact(lines) {
		text = append(text, strconv.Itoa(l))
	}
	return fmt.Errorf("%w; the tree-sitter fallback finds %d declarations matching %q (lines %s) and will not choose between them",
		lspErr, len(matches), namePath, strings.Join(text, ", "))
}

// topologyNodeOfSymbol finds, in a fresh parse, the node of sym — a symbol the
// caller has ALREADY resolved, from the language server or from this fallback:
// the node of the same name whose span starts on the symbol's first line. It never
// resolves a name_path itself, so it cannot answer with a different symbol than
// the one being edited. Where several nodes of that name start on the line (a
// one-line class holding two members called run) the symbol's column and then its
// kind single one out. nil when neither singles out exactly one, and the caller
// line-scans.
func topologyNodeOfSymbol(nodes []topology.Node, sym *protocol.DocumentSymbol) *topology.Node {
	name := symbolBareName(sym.Name)
	line := int(sym.Range.Start.Line) + 1
	var same []*topology.Node
	for i := range nodes {
		if nodes[i].Name == name && nodes[i].StartLine == line {
			same = append(same, &nodes[i])
		}
	}
	switch len(same) {
	case 0:
		return nil
	case 1:
		return same[0]
	}
	if n := soleOf(same, func(n *topology.Node) bool { return n.HasBytes && n.StartCol == int(sym.Range.Start.Character) }); n != nil {
		return n
	}
	return soleOf(same, func(n *topology.Node) bool { return topoKindToSymbolKind(n.Kind) == sym.Kind })
}

// soleOf returns the one node of nodes keep accepts, or nil when none or several do.
func soleOf(nodes []*topology.Node, keep func(*topology.Node) bool) *topology.Node {
	var found *topology.Node
	for _, n := range nodes {
		if !keep(n) {
			continue
		}
		if found != nil {
			return nil
		}
		found = n
	}
	return found
}

// symbolBareName is a document symbol's own name: a Go method "(*S).Run" is Run,
// and a name carrying its argument list ("show()") loses it, as symbolNameMatches does.
func symbolBareName(name string) string {
	if _, method, ok := goMethodReceiver(name); ok {
		return method
	}
	return baseSymbolName(name)
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

// qualifiedSegments splits a Qualified name into its identifier segments.
func qualifiedSegments(qualified string) []string {
	return strings.FieldsFunc(qualified, func(r rune) bool {
		return r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}
