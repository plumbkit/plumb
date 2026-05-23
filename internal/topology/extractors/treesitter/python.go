// Package treesitter provides gotreesitter-backed topology extractors. It is
// pure Go (no CGo). Compared with the legacy regex extractors it tracks real
// class/function nesting, records accurate end lines, and emits certain
// (confidence 1.0) containment edges; intra-file call edges remain name-resolved
// heuristics (confidence 0.8) because tree-sitter is syntactic, not semantic.
package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/golimpio/plumb/internal/topology"
)

// PythonExtractor extracts Python symbols using the gotreesitter Python grammar.
//
// Concurrency: stateless after construction and safe for concurrent use; a fresh
// parser is created per Extract call because gotreesitter parsers are not safe
// for concurrent reuse.
type PythonExtractor struct {
	lang *tsg.Language
}

// NewPython returns a tree-sitter-backed Python extractor.
func NewPython() *PythonExtractor {
	return &PythonExtractor{lang: grammars.PythonLanguage()}
}

func (e *PythonExtractor) Language() string     { return "python" }
func (e *PythonExtractor) Extensions() []string { return []string{".py"} }

// Extract parses src and returns Python classes, functions, methods, tests and
// imports, plus class→method containment edges and intra-file call edges.
// Returns (nil, nil, nil) when the source cannot be parsed.
func (e *PythonExtractor) Extract(_ context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	tree, err := tsg.NewParser(e.lang).Parse(src)
	if err != nil || tree == nil {
		return nil, nil, nil
	}
	w := &pyWalk{lang: e.lang, src: src, path: relPath, funcIdx: map[string]int64{}}
	w.walk(tree.RootNode(), -1)
	w.callEdges(tree.RootNode())
	return w.nodes, w.edges, nil
}

type pyWalk struct {
	lang    *tsg.Language
	src     []byte
	path    string
	nodes   []topology.Node
	edges   []topology.Edge
	funcIdx map[string]int64 // function/method/test name → node index, for call edges
}

func line(p tsg.Point) int { return int(p.Row) + 1 }

func (w *pyWalk) fieldName(n *tsg.Node) string {
	if nm := n.ChildByFieldName("name", w.lang); nm != nil {
		return nm.Text(w.src)
	}
	return ""
}

func (w *pyWalk) walk(n *tsg.Node, enclosingClass int64) {
	switch n.Type(w.lang) {
	case "class_definition":
		idx := w.addClass(n)
		w.walkChildren(n, idx)
	case "function_definition":
		w.addFunc(n, enclosingClass)
		// Definitions nested inside a function are not methods of enclosingClass.
		w.walkChildren(n, -1)
	case "import_statement", "import_from_statement":
		w.addImports(n)
	default:
		w.walkChildren(n, enclosingClass)
	}
}

func (w *pyWalk) walkChildren(n *tsg.Node, enclosingClass int64) {
	for _, c := range n.Children() {
		w.walk(c, enclosingClass)
	}
}

func (w *pyWalk) addClass(n *tsg.Node) int64 {
	name := w.fieldName(n)
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      topology.KindClass,
		Name:      name,
		Qualified: name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "python",
		Path:      w.path,
	})
	return idx
}

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
	w.nodes = append(w.nodes, topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "python",
		Path:      w.path,
	})
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
	w.nodes = append(w.nodes, topology.Node{
		Kind:      topology.KindImport,
		Name:      name,
		StartLine: line(n.StartPoint()),
		Language:  "python",
		Path:      w.path,
	})
}

// callEdges does a second pass emitting EdgeCalls between functions defined in
// the same file. The call node is syntactically certain but the callee is
// resolved by name within the file, so confidence is 0.8 (heuristic).
func (w *pyWalk) callEdges(root *tsg.Node) {
	seen := map[[2]int64]bool{}
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
	w.edges = append(w.edges, topology.Edge{
		FromID:     curFunc,
		ToID:       to,
		Kind:       topology.EdgeCalls,
		Confidence: 0.8,
		Source:     "heuristic",
	})
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
