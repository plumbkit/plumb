package treesitter

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	tsg "github.com/odvcencio/gotreesitter"

	"github.com/plumbkit/plumb/internal/topology"
)

// extractWith is the parse envelope every extractor in this package shares:
// parse src, treat an unparseable file as "nothing to index" rather than an
// error, hand the root to walk, and — crucially — release the parse arena back
// to gotreesitter's pool on the way out.
//
// That last step is the reason this exists. The package's memory discipline
// depends on every extractor deferring tree.Release(); a new extractor that
// forgets leaks an arena per file indexed, which shows up as unbounded daemon
// growth on a large resync rather than as a test failure. Sixteen hand-written
// copies of the envelope made that a matter of remembering. One copy makes it
// structural: an extractor written against extractWith cannot forget, because it
// never holds the tree.
//
// It also bounds the parse by ctx's deadline. A grammar's error recovery can go
// superlinear on a file well inside the indexer's size caps, so without this a
// single pathological file would run for as long as it liked. Note that a
// timed-out parse comes back as a PARTIAL tree with a nil error — walking it
// would record a truncated symbol set as though it were the whole file — so the
// early stop is turned into an error for the caller to record.
//
// A dead context starts no parse, whether it died of cancellation or of its
// deadline — the same contract wasmts.Extract and the safeExtract watchdog above
// it enforce. The two checks are not redundant: ctx.Err() is the only one that
// sees a cancelled context (which usually carries no deadline at all), and the
// budget check is the only one that sees a deadline that has passed but whose
// context has not been marked yet.
//
// walk returns the nodes and edges it collected; a nil edge slice is fine for a
// language that emits none.
func extractWith(
	ctx context.Context,
	lang *tsg.Language,
	src []byte,
	walk func(root *tsg.Node) ([]topology.Node, []topology.Edge),
) ([]topology.Node, []topology.Edge, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	parser := tsg.NewParser(lang)
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, nil, context.DeadlineExceeded
		}
		//nolint:gosec // G115: remaining is > 0 per the check above, so the conversion cannot wrap.
		parser.SetTimeoutMicros(uint64(remaining.Microseconds()))
	}
	tree, err := parser.Parse(src)
	if err != nil || tree == nil {
		return nil, nil, nil
	}
	defer tree.Release()
	if tree.ParseStoppedEarly() {
		return nil, nil, fmt.Errorf("parse stopped early: %s", tree.ParseStopReason())
	}
	nodes, edges := walk(tree.RootNode())
	return nodes, edges, nil
}

// appendTest emits a KindTest node spanning call, stamped with its byte span.
// Shared by the JavaScript and TypeScript walks, whose test emission is
// otherwise identical clone code.
func appendTest(nodes []topology.Node, name, lang, path string, call *tsg.Node) []topology.Node {
	node := topology.Node{
		Kind:      topology.KindTest,
		Name:      name,
		Qualified: name,
		StartLine: line(call.StartPoint()),
		EndLine:   line(call.EndPoint()),
		Language:  lang,
		Path:      path,
	}
	setSpan(&node, call)
	return append(nodes, node)
}

// walkCallSites drives the second pass shared by every extractor that emits
// intra-file call edges: a pre-order descent that threads the innermost
// enclosing callable's node index down through the children.
//
// enclosing is given each node and the current enclosing index, and returns the
// index in scope for that node's subtree — the current one unchanged for a node
// that opens no callable scope. visit is then called for every node with the
// resolved scope; a call-site handler tests the node type itself.
//
// The traversal is what is worth sharing: the descent must seed with -1 ("no
// enclosing callable"), must resolve the scope BEFORE visiting so a call in a
// function body attributes to that function, and must pass the resolved index to
// children rather than the parent's. Each of the nine extractors carried its own
// copy of that; a language whose copy seeded or ordered it differently would
// silently emit no call edges at all — an absence no test notices, since a
// missing heuristic edge is indistinguishable from a file with no calls.
func walkCallSites(root *tsg.Node, enclosing func(n *tsg.Node, cur int64) int64, visit func(n *tsg.Node, cur int64)) {
	var rec func(n *tsg.Node, cur int64)
	rec = func(n *tsg.Node, cur int64) {
		cur = enclosing(n, cur)
		visit(n, cur)
		for _, c := range n.Children() {
			rec(c, cur)
		}
	}
	rec(root, -1)
}

// scopeByType builds an `enclosing` function for the common case: a language
// whose callable scopes are a flat set of node types, each carrying its name
// where nameOf can read it. funcIdx maps that name to the emitted node index.
//
// Languages whose scopes are not a flat type switch (JavaScript and TypeScript,
// where an arrow function bound to a const opens a scope) pass their own
// enclosing function to walkCallSites instead.
func scopeByType(lang *tsg.Language, funcIdx map[string]int64, nameOf func(n *tsg.Node) string, types ...string) func(*tsg.Node, int64) int64 {
	return func(n *tsg.Node, cur int64) int64 {
		typ := n.Type(lang)
		for _, t := range types {
			if typ != t {
				continue
			}
			if idx, ok := funcIdx[nameOf(n)]; ok {
				return idx
			}
			break
		}
		return cur
	}
}

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
// non-comment node and no blank line anywhere in the run). Returns (0, 0) — the
// "no doc span" sentinel — when there is no such block. isComment reports
// whether a node type is a comment in the grammar (it varies: "comment",
// "line_comment", "block_comment", …).
//
// Flushness is not decoration, it is the whole safety property. Everything
// downstream treats a doc span as part of the symbol —
// docCommentStartPreferTopology prefers it over the line-scan heuristic, and
// move_symbol's include_doc_comment defaults true — so a span that reaches back
// across a blank line to a file-leading SPDX/licence banner is a silently
// deleted licence header on the next replace_symbol_body. Both ends of the run
// are therefore checked: the closest comment must be flush against the
// declaration, and the backward walk stops at the first separation, so
// `banner / blank line / doc-block / decl` keeps only the doc-block.
//
// The backward walk goes through the DECLARATION's sibling list by index rather
// than by chaining PrevSibling off each comment, because a comment cannot be
// relied on to know where it sits. In the tree-sitter JavaScript grammar EVERY
// comment ahead of the first non-comment top-level node reports a nil Parent —
// the whole leading run, not just its first line — so PrevSibling, which
// resolves through the parent link, returns nil from the first hop on any of
// them and the run collapses to its last line: a three-line `//` doc block above
// the first declaration in a .js file yielded only its third line, which
// include_doc_comment then split, moving half a comment. A run further down the
// file has sound parent links, which is why only the leading one was affected.
// The declaration's own parent link is sound in both positions (that is how last
// was found at all), and its child list contains the whole run, so resolving the
// run there is immune to the quirk in every grammar.
func docSpanBefore(decl *tsg.Node, lang *tsg.Language, src []byte, isComment func(typ string) bool) (start, end int) {
	last := decl.PrevSibling() // the comment closest to the declaration
	if last == nil || !isComment(last.Type(lang)) || !commentFlushBefore(src, last, decl.StartByte()) {
		return 0, 0
	}
	first := last
	sibs := siblingsOf(decl)
	for j := nodeIndexIn(sibs, last) - 1; j >= 0; j-- {
		sib := sibs[j]
		if !isComment(sib.Type(lang)) || !commentFlushBefore(src, sib, first.StartByte()) {
			break
		}
		first = sib
	}
	return clampU32(first.StartByte()), clampU32(last.EndByte())
}

// siblingsOf returns the child list n belongs to — its parent's children, n
// included — or nil when n has no parent.
func siblingsOf(n *tsg.Node) []*tsg.Node {
	parent := n.Parent()
	if parent == nil {
		return nil
	}
	return parent.Children()
}

// nodeIndexIn returns target's position in sibs, or -1 when it is not there (in
// particular for a nil sibs, which makes every caller's index walk a no-op).
//
// Nodes are matched on their byte span, not by pointer: gotreesitter
// materializes node structs lazily and on demand, so the same syntactic node
// reached two different ways is not guaranteed to be the same *Node. A span is
// a safe key here because siblings never overlap.
func nodeIndexIn(sibs []*tsg.Node, target *tsg.Node) int {
	for i, s := range sibs {
		if s.StartByte() == target.StartByte() && s.EndByte() == target.EndByte() {
			return i
		}
	}
	return -1
}

// commentFlushBefore reports whether comment sits directly above whatever starts
// at byte offset next, with no blank line between the two.
//
// The blank line is invisible to the node tree, and in two different ways. No
// grammar emits a node for it, so a bare previous-sibling scan walks past one
// without noticing; and several grammars let a comment node swallow the newlines
// that follow it, which defeats a raw byte gap too. Rust is the sharp case:
// `/// banner\n\npub fn f()` parses as a line_comment spanning [1:0]–[3:0] whose
// EndByte IS the function's StartByte — blank line and all, inside the comment
// node. Only the source text can answer the question, so the comment's trailing
// whitespace is trimmed off first and everything from there to next must hold at
// most one newline. Same row (`export /** … */ class C {}`) and the row directly
// above both pass; anything further does not.
func commentFlushBefore(src []byte, comment *tsg.Node, next uint32) bool {
	lo, hi, to := clampU32(comment.StartByte()), clampU32(comment.EndByte()), clampU32(next)
	if hi > len(src) {
		hi = len(src)
	}
	if to > len(src) || lo > hi || to < lo {
		return false
	}
	hi = lo + len(bytes.TrimRight(src[lo:hi], " \t\r\n\v\f"))
	if to < hi {
		return false
	}
	return bytes.Count(src[hi:to], []byte{'\n'}) <= 1
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
