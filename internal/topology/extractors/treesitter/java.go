package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// JavaExtractor extracts Java symbols using the gotreesitter Java grammar.
//
// Concurrency: stateless after construction and safe for concurrent use; a
// fresh parser is created per Extract call because gotreesitter parsers are not
// safe for concurrent reuse.
type JavaExtractor struct {
	lang lazyGrammar
}

// NewJava returns a tree-sitter-backed Java extractor.
func NewJava() *JavaExtractor {
	return &JavaExtractor{lang: lazyGrammar{load: grammars.JavaLanguage}}
}

func (e *JavaExtractor) Language() string     { return "java" }
func (e *JavaExtractor) Extensions() []string { return []string{".java"} }

// Extract parses src and returns Java classes/records/enums (KindClass),
// interfaces (KindType — a contract, mirroring the Rust trait / Kotlin interface
// mapping), methods and constructors, fields (final → constant, otherwise
// variable), enum constants (constants), imports, and @Test-annotated tests,
// plus container → member containment edges and intra-file call edges.
// Containment is lexical and certain (1.0/extractor); intra-file calls are
// name-resolved heuristics (0.8). This is the structural Map for Java; the
// semantic GPS is the jdtls LSP adapter. Returns (nil, nil, nil) when src cannot
// be parsed.
func (e *JavaExtractor) Extract(_ context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	tree, err := tsg.NewParser(e.lang.get()).Parse(src)
	if err != nil || tree == nil {
		return nil, nil, nil
	}
	defer tree.Release()
	w := &javaWalk{lang: e.lang.get(), src: src, path: relPath, funcIdx: map[string]int64{}}
	w.walk(tree.RootNode(), -1, false)
	w.callEdges(tree.RootNode())
	return w.nodes, w.edges, nil
}

type javaWalk struct {
	lang    *tsg.Language
	src     []byte
	path    string
	nodes   []topology.Node
	edges   []topology.Edge
	funcIdx map[string]int64 // method/constructor name → node index, for call edges
}

// walk descends the tree. enclosing is the node index of the lexically enclosing
// type (-1 at file scope); inFunc is true once inside a method/constructor body
// (suppresses local declarations).
func (w *javaWalk) walk(n *tsg.Node, enclosing int64, inFunc bool) {
	switch n.Type(w.lang) {
	case "class_declaration", "record_declaration":
		w.handleType(n, topology.KindClass, enclosing)
	case "enum_declaration":
		w.handleType(n, topology.KindClass, enclosing)
	case "interface_declaration":
		w.handleType(n, topology.KindType, enclosing)
	case "method_declaration", "constructor_declaration":
		w.addMethod(n, enclosing)
		w.walkChildren(n, -1, true)
	case "field_declaration":
		if !inFunc {
			w.addFields(n, enclosing)
		}
	case "enum_constant":
		if !inFunc {
			w.addEnumConstant(n, enclosing)
		}
	case "import_declaration":
		w.addImport(n)
	default:
		w.walkChildren(n, enclosing, inFunc)
	}
}

func (w *javaWalk) walkChildren(n *tsg.Node, enclosing int64, inFunc bool) {
	for _, c := range n.Children() {
		w.walk(c, enclosing, inFunc)
	}
}

// handleType adds a class/record/enum (KindClass) or interface (KindType) node
// and walks its body, attributing members to it.
func (w *javaWalk) handleType(n *tsg.Node, kind topology.NodeKind, enclosing int64) {
	name := w.declName(n)
	if name == "" {
		w.walkChildren(n, enclosing, false)
		return
	}
	idx := w.addNode(n, kind, name, enclosing)
	if body := w.typeBody(n); body != nil {
		w.walkChildren(body, idx, false)
	}
}

func (w *javaWalk) addMethod(n *tsg.Node, enclosing int64) {
	name := w.declName(n)
	if name == "" {
		return
	}
	kind := topology.KindFunction
	if enclosing >= 0 {
		kind = topology.KindMethod
	}
	if w.isTest(n) {
		kind = topology.KindTest
	}
	idx := w.addNode(n, kind, name, enclosing)
	w.funcIdx[name] = idx
}

// addFields records each variable declared in a field_declaration. A field with
// a `final` modifier is a constant; otherwise a variable.
func (w *javaWalk) addFields(n *tsg.Node, enclosing int64) {
	kind := topology.KindVariable
	if m := childByType(n, "modifiers", w.lang); m != nil && strings.Contains(m.Text(w.src), "final") {
		kind = topology.KindConstant
	}
	for _, c := range n.Children() {
		if c.Type(w.lang) != "variable_declarator" {
			continue
		}
		name := w.fieldName(c)
		if name == "" {
			continue
		}
		w.addNode(n, kind, name, enclosing)
	}
}

func (w *javaWalk) addEnumConstant(n *tsg.Node, enclosing int64) {
	name := w.declName(n)
	if name == "" {
		if id := childByType(n, "identifier", w.lang); id != nil {
			name = id.Text(w.src)
		}
	}
	if name == "" {
		return
	}
	w.addNode(n, topology.KindConstant, name, enclosing)
}

func (w *javaWalk) addImport(n *tsg.Node) {
	name := ""
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "scoped_identifier", "identifier":
			name = c.Text(w.src)
		}
	}
	if name == "" {
		return
	}
	w.nodes = append(w.nodes, topology.Node{
		Kind:      topology.KindImport,
		Name:      strings.TrimSpace(name),
		StartLine: line(n.StartPoint()),
		Language:  "java",
		Path:      w.path,
	})
}

// addNode appends a node and, when it has a lexical enclosing type, a certain
// (1.0/extractor) containment edge.
func (w *javaWalk) addNode(n *tsg.Node, kind topology.NodeKind, name string, enclosing int64) int64 {
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "java",
		Path:      w.path,
	})
	if enclosing >= 0 {
		w.edges = append(w.edges, topology.Edge{
			FromID:     enclosing,
			ToID:       idx,
			Kind:       topology.EdgeContains,
			Confidence: 1.0,
			Source:     "extractor",
		})
	}
	return idx
}

// declName returns a declaration's `name` field (class/interface/enum/record/
// method/constructor/enum-constant all carry one in the Java grammar).
func (w *javaWalk) declName(n *tsg.Node) string {
	if nm := n.ChildByFieldName("name", w.lang); nm != nil {
		return nm.Text(w.src)
	}
	return ""
}

func (w *javaWalk) fieldName(vd *tsg.Node) string {
	if nm := vd.ChildByFieldName("name", w.lang); nm != nil {
		return nm.Text(w.src)
	}
	if id := childByType(vd, "identifier", w.lang); id != nil {
		return id.Text(w.src)
	}
	return ""
}

func (w *javaWalk) typeBody(n *tsg.Node) *tsg.Node {
	for _, t := range []string{"class_body", "interface_body", "enum_body"} {
		if b := childByType(n, t, w.lang); b != nil {
			return b
		}
	}
	return nil
}

// isTest reports whether a method carries a @Test-family annotation (a bare
// `Test` or any name ending in `Test`, e.g. ParameterizedTest).
func (w *javaWalk) isTest(n *tsg.Node) bool {
	mods := childByType(n, "modifiers", w.lang)
	if mods == nil {
		return false
	}
	for _, c := range mods.Children() {
		switch c.Type(w.lang) {
		case "marker_annotation", "annotation":
			name := lastJavaIdentifier(c, w.lang, w.src)
			if name == "Test" || strings.HasSuffix(name, "Test") {
				return true
			}
		}
	}
	return false
}

// callEdges does a second pass emitting EdgeCalls between methods defined in the
// file. The call site is syntactically certain but the callee is resolved by
// name within the file, so confidence is 0.8 (heuristic).
func (w *javaWalk) callEdges(root *tsg.Node) {
	seen := map[[2]int64]bool{}
	var rec func(n *tsg.Node, curFunc int64)
	rec = func(n *tsg.Node, curFunc int64) {
		switch n.Type(w.lang) {
		case "method_declaration", "constructor_declaration":
			if idx, ok := w.funcIdx[w.declName(n)]; ok {
				curFunc = idx
			}
		case "method_invocation":
			w.maybeCallEdge(n, curFunc, seen)
		}
		for _, c := range n.Children() {
			rec(c, curFunc)
		}
	}
	rec(root, -1)
}

func (w *javaWalk) maybeCallEdge(call *tsg.Node, curFunc int64, seen map[[2]int64]bool) {
	if curFunc < 0 {
		return
	}
	nm := call.ChildByFieldName("name", w.lang)
	if nm == nil {
		return
	}
	to, ok := w.funcIdx[nm.Text(w.src)]
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

// lastJavaIdentifier returns the deepest-rightmost identifier under n, so a
// qualified annotation like `@org.junit.Test` yields its simple name "Test".
func lastJavaIdentifier(n *tsg.Node, lang *tsg.Language, src []byte) string {
	var last string
	var rec func(*tsg.Node)
	rec = func(m *tsg.Node) {
		if m.Type(lang) == "identifier" {
			last = m.Text(src)
		}
		for _, c := range m.Children() {
			rec(c)
		}
	}
	rec(n)
	return last
}
