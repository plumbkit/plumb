// Package treesitter provides gotreesitter-backed topology extractors. It is
// pure Go (no CGo). Compared with the legacy regex extractors it tracks real
// class/function nesting, records accurate end lines, and emits certain
// (confidence 1.0) containment edges; intra-file call edges remain name-resolved
// heuristics (confidence 0.8) because tree-sitter is syntactic, not semantic.
package treesitter

import (
	"context"
	"strings"
	"unicode"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// PythonExtractor extracts Python symbols using the gotreesitter Python grammar.
//
// Concurrency: stateless after construction and safe for concurrent use; a fresh
// parser is created per Extract call because gotreesitter parsers are not safe
// for concurrent reuse.
type PythonExtractor struct {
	lang lazyGrammar
}

// NewPython returns a tree-sitter-backed Python extractor.
func NewPython() *PythonExtractor {
	return &PythonExtractor{lang: lazyGrammar{load: grammars.PythonLanguage}}
}

func (e *PythonExtractor) Language() string     { return "python" }
func (e *PythonExtractor) Extensions() []string { return []string{".py"} }

// Extract parses src and returns Python classes, functions, methods, tests,
// imports, and module- and class-level bindings (ALL_CAPS → constant, else
// variable), plus class→member containment edges and intra-file call edges.
// Function-local bindings are not surfaced. Returns (nil, nil, nil) when the
// source cannot be parsed.
func (e *PythonExtractor) Extract(_ context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	tree, err := tsg.NewParser(e.lang.get()).Parse(src)
	if err != nil || tree == nil {
		return nil, nil, nil
	}
	defer tree.Release()
	w := &pyWalk{lang: e.lang.get(), src: src, path: relPath, funcIdx: map[string]int64{}}
	w.walk(tree.RootNode(), -1, false)
	w.callEdges(tree.RootNode())
	return w.nodes, w.edges, nil
}

type pyWalk struct {
	lang       *tsg.Language
	src        []byte
	path       string
	nodes      []topology.Node
	edges      []topology.Edge
	funcIdx    map[string]int64 // function/method/test name → node index, for call edges
	nameCounts map[string]int   // callable Name → count, for ambiguous-call down-weight (#30)
}

func line(p tsg.Point) int { return int(p.Row) + 1 }

func (w *pyWalk) fieldName(n *tsg.Node) string {
	if nm := n.ChildByFieldName("name", w.lang); nm != nil {
		return nm.Text(w.src)
	}
	return ""
}

func (w *pyWalk) walk(n *tsg.Node, enclosingClass int64, inFunc bool) {
	switch n.Type(w.lang) {
	case "class_definition":
		idx := w.addClass(n)
		w.walkChildren(n, idx, inFunc)
	case "function_definition":
		w.addFunc(n, enclosingClass)
		// Definitions and bindings nested in a function are locals, not
		// methods/attributes of enclosingClass.
		w.walkChildren(n, -1, true)
	case "import_statement", "import_from_statement":
		w.addImports(n)
	case "assignment":
		if !inFunc {
			w.maybeAssignment(n, enclosingClass)
		}
	default:
		w.walkChildren(n, enclosingClass, inFunc)
	}
}

func (w *pyWalk) walkChildren(n *tsg.Node, enclosingClass int64, inFunc bool) {
	for _, c := range n.Children() {
		w.walk(c, enclosingClass, inFunc)
	}
}

// maybeAssignment records a module- or class-level binding from an `assignment`
// node: KindConstant when the name is ALL_CAPS (Python's constant convention),
// else KindVariable. Only simple identifier targets are recorded (tuple,
// attribute and subscript targets are skipped). A class-level binding gains a
// certain (1.0/extractor) containment edge.
func (w *pyWalk) maybeAssignment(asn *tsg.Node, enclosingClass int64) {
	left := asn.ChildByFieldName("left", w.lang)
	if left == nil || left.Type(w.lang) != "identifier" {
		return
	}
	name := left.Text(w.src)
	kind := topology.KindVariable
	if isConstName(name) {
		kind = topology.KindConstant
	}
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		StartLine: line(asn.StartPoint()),
		EndLine:   line(asn.EndPoint()),
		Language:  "python",
		Path:      w.path,
	}
	setSpan(&node, asn)
	w.nodes = append(w.nodes, node)
	if enclosingClass >= 0 {
		w.edges = append(w.edges, topology.Edge{
			FromID:     enclosingClass,
			ToID:       idx,
			Kind:       topology.EdgeContains,
			Confidence: 1.0,
			Source:     "extractor",
		})
	}
}

// isConstName reports whether a Python name follows the ALL_CAPS constant
// convention (every cased letter is upper-case, with at least one letter).
func isConstName(name string) bool {
	hasLetter := false
	for _, r := range name {
		if unicode.IsLetter(r) {
			hasLetter = true
			if !unicode.IsUpper(r) {
				return false
			}
		}
	}
	return hasLetter
}

func (w *pyWalk) addClass(n *tsg.Node) int64 {
	name := w.fieldName(n)
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      topology.KindClass,
		Name:      name,
		Qualified: name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "python",
		Path:      w.path,
	}
	setSpan(&node, n)
	node.DocStartByte, node.DocEndByte = docSpanBefore(n, w.lang, pyIsComment)
	w.nodes = append(w.nodes, node)
	return idx
}

// pyIsComment reports whether a Python grammar node type is a comment.
func pyIsComment(typ string) bool { return typ == "comment" }

func (w *pyWalk) addFunc(n *tsg.Node, enclosingClass int64) {
	name := w.fieldName(n)
	if name == "" {
		return
	}
	kind := topology.KindFunction
	if enclosingClass >= 0 {
		kind = topology.KindMethod
	}
	if isTestName(name) {
		kind = topology.KindTest
	}
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "python",
		Path:      w.path,
	}
	setSpan(&node, n)
	node.DocStartByte, node.DocEndByte = docSpanBefore(n, w.lang, pyIsComment)
	w.nodes = append(w.nodes, node)
	w.funcIdx[name] = idx
	if enclosingClass >= 0 {
		w.edges = append(w.edges, topology.Edge{
			FromID:     enclosingClass,
			ToID:       idx,
			Kind:       topology.EdgeContains,
			Confidence: 1.0,
			Source:     "extractor",
		})
	}
}

func (w *pyWalk) addImports(n *tsg.Node) {
	switch n.Type(w.lang) {
	case "import_from_statement":
		if m := n.ChildByFieldName("module_name", w.lang); m != nil {
			w.addImport(m.Text(w.src), n)
		}
	case "import_statement":
		for _, c := range n.Children() {
			switch c.Type(w.lang) {
			case "dotted_name":
				w.addImport(c.Text(w.src), n)
			case "aliased_import":
				if nm := c.ChildByFieldName("name", w.lang); nm != nil {
					w.addImport(nm.Text(w.src), n)
				}
			}
		}
	}
}

func (w *pyWalk) addImport(name string, n *tsg.Node) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	node := topology.Node{
		Kind:      topology.KindImport,
		Name:      name,
		StartLine: line(n.StartPoint()),
		Language:  "python",
		Path:      w.path,
	}
	setSpan(&node, n)
	w.nodes = append(w.nodes, node)
}

// callEdges does a second pass emitting EdgeCalls between functions defined in
// the same file. The call node is syntactically certain but the callee is
// resolved by name within the file, so confidence is 0.8 (heuristic).
func (w *pyWalk) callEdges(root *tsg.Node) {
	seen := map[[2]int64]bool{}
	w.nameCounts = callableNameCounts(w.nodes)
	var rec func(n *tsg.Node, curFunc int64)
	rec = func(n *tsg.Node, curFunc int64) {
		switch n.Type(w.lang) {
		case "function_definition":
			if idx, ok := w.funcIdx[w.fieldName(n)]; ok {
				curFunc = idx
			}
		case "call":
			w.maybeCallEdge(n, curFunc, seen)
		}
		for _, c := range n.Children() {
			rec(c, curFunc)
		}
	}
	rec(root, -1)
}

func (w *pyWalk) maybeCallEdge(call *tsg.Node, curFunc int64, seen map[[2]int64]bool) {
	if curFunc < 0 {
		return
	}
	fn := call.ChildByFieldName("function", w.lang)
	if fn == nil {
		return
	}
	to, ok := w.funcIdx[w.calleeName(fn)]
	if !ok || to == curFunc {
		return
	}
	key := [2]int64{curFunc, to}
	if seen[key] {
		return
	}
	seen[key] = true
	w.edges = append(w.edges, heuristicCallEdge(curFunc, to, w.nodes, w.nameCounts))
}

func (w *pyWalk) calleeName(fn *tsg.Node) string {
	switch fn.Type(w.lang) {
	case "identifier":
		return fn.Text(w.src)
	case "attribute":
		if a := fn.ChildByFieldName("attribute", w.lang); a != nil {
			return a.Text(w.src)
		}
	}
	return ""
}

func isTestName(name string) bool {
	return strings.HasPrefix(name, "test_") || strings.HasPrefix(name, "Test")
}
