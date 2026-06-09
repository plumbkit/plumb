package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// KotlinExtractor extracts Kotlin symbols using the gotreesitter Kotlin grammar.
//
// Concurrency: stateless after construction and safe for concurrent use; a
// fresh parser is created per Extract call because gotreesitter parsers are not
// safe for concurrent reuse.
type KotlinExtractor struct {
	lang lazyGrammar
}

// NewKotlin returns a tree-sitter-backed Kotlin extractor.
func NewKotlin() *KotlinExtractor {
	return &KotlinExtractor{lang: lazyGrammar{load: grammars.KotlinLanguage}}
}

func (e *KotlinExtractor) Language() string     { return "kotlin" }
func (e *KotlinExtractor) Extensions() []string { return []string{".kt", ".kts"} }

// Extract parses src and returns Kotlin classes (class/data/sealed/enum class,
// object, companion object), interfaces, functions, methods, member and
// top-level properties (val/const → constant, var → variable), enum entries,
// imports and `@Test`-annotated tests, plus container → member containment
// edges and intra-file call edges. Containment is lexical (the member is inside
// the class/object body) and therefore certain (1.0/extractor); intra-file call
// edges are name-resolved within the file and so are heuristic (0.8). Interfaces
// are emitted as KindType (a type contract, mirroring the Rust trait mapping);
// concrete declarations are KindClass. Returns (nil, nil, nil) when src cannot
// be parsed.
func (e *KotlinExtractor) Extract(_ context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	tree, err := tsg.NewParser(e.lang.get()).Parse(src)
	if err != nil || tree == nil {
		return nil, nil, nil
	}
	defer tree.Release()
	w := &kotlinWalk{lang: e.lang.get(), src: src, path: relPath, funcIdx: map[string]int64{}}
	w.walk(tree.RootNode(), -1, false)
	w.callEdges(tree.RootNode())
	return w.nodes, w.edges, nil
}

type kotlinWalk struct {
	lang    *tsg.Language
	src     []byte
	path    string
	nodes   []topology.Node
	edges   []topology.Edge
	funcIdx map[string]int64 // function/method/test name → node index, for call edges
}

// walk descends the tree. enclosing is the node index of the lexically
// enclosing class/object (-1 at file scope); inFunc is true once inside a
// function body, which suppresses extraction of local properties and nested
// declarations as members.
func (w *kotlinWalk) walk(n *tsg.Node, enclosing int64, inFunc bool) {
	switch n.Type(w.lang) {
	case "class_declaration":
		w.handleClass(n, enclosing)
	case "object_declaration", "companion_object":
		w.handleObject(n, enclosing)
	case "function_declaration":
		w.addFunc(n, enclosing)
		// Members declared inside a function body are not members of `enclosing`.
		w.walkChildren(n, -1, true)
	case "property_declaration":
		if !inFunc {
			w.addProperty(n, enclosing)
		}
	case "enum_entry":
		if !inFunc {
			w.addEnumEntry(n, enclosing)
		}
	case "import_header":
		w.addImport(n)
	default:
		w.walkChildren(n, enclosing, inFunc)
	}
}

func (w *kotlinWalk) walkChildren(n *tsg.Node, enclosing int64, inFunc bool) {
	for _, c := range n.Children() {
		w.walk(c, enclosing, inFunc)
	}
}

// handleClass adds a class/interface/enum node and walks its body, attributing
// members to it.
func (w *kotlinWalk) handleClass(n *tsg.Node, enclosing int64) {
	name := w.typeName(n)
	if name == "" {
		w.walkChildren(n, enclosing, false)
		return
	}
	kind := topology.KindClass
	if w.isInterface(n) {
		kind = topology.KindType
	}
	idx := w.addType(n, name, kind, enclosing)
	if body := w.classBody(n); body != nil {
		w.walkChildren(body, idx, false)
	}
}

// handleObject adds an object/companion-object node (a companion without a name
// is recorded as "Companion") and walks its body.
func (w *kotlinWalk) handleObject(n *tsg.Node, enclosing int64) {
	name := w.typeName(n)
	if name == "" && n.Type(w.lang) == "companion_object" {
		name = "Companion"
	}
	if name == "" {
		w.walkChildren(n, enclosing, false)
		return
	}
	idx := w.addType(n, name, topology.KindClass, enclosing)
	if body := childByType(n, "class_body", w.lang); body != nil {
		w.walkChildren(body, idx, false)
	}
}

func (w *kotlinWalk) addType(n *tsg.Node, name string, kind topology.NodeKind, enclosing int64) int64 {
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "kotlin",
		Path:      w.path,
	})
	w.containedBy(enclosing, idx)
	return idx
}

func (w *kotlinWalk) addFunc(n *tsg.Node, enclosing int64) {
	name := w.funcName(n)
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
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "kotlin",
		Path:      w.path,
	})
	w.funcIdx[name] = idx
	w.containedBy(enclosing, idx)
}

// addProperty records a member or top-level property. val/const → constant,
// var → variable.
func (w *kotlinWalk) addProperty(n *tsg.Node, enclosing int64) {
	vd := childByType(n, "variable_declaration", w.lang)
	if vd == nil {
		return
	}
	id := childByType(vd, "simple_identifier", w.lang)
	if id == nil {
		return
	}
	kind := topology.KindConstant
	if b := childByType(n, "binding_pattern_kind", w.lang); b != nil && strings.TrimSpace(b.Text(w.src)) == "var" {
		kind = topology.KindVariable
	}
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      kind,
		Name:      stripBackticks(id.Text(w.src)),
		Qualified: stripBackticks(id.Text(w.src)),
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "kotlin",
		Path:      w.path,
	})
	w.containedBy(enclosing, idx)
}

func (w *kotlinWalk) addEnumEntry(n *tsg.Node, enclosing int64) {
	id := childByType(n, "simple_identifier", w.lang)
	if id == nil {
		return
	}
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      topology.KindConstant,
		Name:      id.Text(w.src),
		Qualified: id.Text(w.src),
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "kotlin",
		Path:      w.path,
	})
	w.containedBy(enclosing, idx)
}

func (w *kotlinWalk) addImport(n *tsg.Node) {
	id := childByType(n, "identifier", w.lang)
	if id == nil {
		return
	}
	name := strings.TrimSpace(id.Text(w.src))
	if name == "" {
		return
	}
	w.nodes = append(w.nodes, topology.Node{
		Kind:      topology.KindImport,
		Name:      name,
		StartLine: line(n.StartPoint()),
		Language:  "kotlin",
		Path:      w.path,
	})
}

// containedBy emits a certain (1.0/extractor) containment edge when child has a
// lexical enclosing declaration.
func (w *kotlinWalk) containedBy(enclosing, child int64) {
	if enclosing < 0 {
		return
	}
	w.edges = append(w.edges, topology.Edge{
		FromID:     enclosing,
		ToID:       child,
		Kind:       topology.EdgeContains,
		Confidence: 1.0,
		Source:     "extractor",
	})
}

func (w *kotlinWalk) typeName(n *tsg.Node) string {
	if id := childByType(n, "type_identifier", w.lang); id != nil {
		return id.Text(w.src)
	}
	return ""
}

// funcName returns the function's declared name (its direct simple_identifier
// child, which follows any modifiers and receiver type), with backticks
// stripped from quoted names.
func (w *kotlinWalk) funcName(n *tsg.Node) string {
	if id := childByType(n, "simple_identifier", w.lang); id != nil {
		return stripBackticks(id.Text(w.src))
	}
	return ""
}

// isInterface reports whether a class_declaration is an `interface` (the keyword
// is an unnamed child token; `class`/`enum class` are not).
func (w *kotlinWalk) isInterface(n *tsg.Node) bool {
	for _, c := range n.Children() {
		if !c.IsNamed() && c.Type(w.lang) == "interface" {
			return true
		}
	}
	return false
}

// isTest reports whether a function carries a `@Test`-family annotation
// (matches a bare `Test` or any name ending in `Test`, e.g. ParameterizedTest).
func (w *kotlinWalk) isTest(n *tsg.Node) bool {
	mods := childByType(n, "modifiers", w.lang)
	if mods == nil {
		return false
	}
	for _, c := range mods.Children() {
		if c.Type(w.lang) != "annotation" {
			continue
		}
		name := lastTypeIdentifier(c, w.lang, w.src)
		if name == "Test" || strings.HasSuffix(name, "Test") {
			return true
		}
	}
	return false
}

func (w *kotlinWalk) classBody(n *tsg.Node) *tsg.Node {
	if b := childByType(n, "class_body", w.lang); b != nil {
		return b
	}
	return childByType(n, "enum_class_body", w.lang)
}

// callEdges does a second pass emitting EdgeCalls between functions defined in
// the file. The call site is syntactically certain but the callee is resolved
// by name within the file, so confidence is 0.8 (heuristic).
func (w *kotlinWalk) callEdges(root *tsg.Node) {
	seen := map[[2]int64]bool{}
	var rec func(n *tsg.Node, curFunc int64)
	rec = func(n *tsg.Node, curFunc int64) {
		switch n.Type(w.lang) {
		case "function_declaration":
			if idx, ok := w.funcIdx[w.funcName(n)]; ok {
				curFunc = idx
			}
		case "call_expression":
			w.maybeCallEdge(n, curFunc, seen)
		}
		for _, c := range n.Children() {
			rec(c, curFunc)
		}
	}
	rec(root, -1)
}

func (w *kotlinWalk) maybeCallEdge(call *tsg.Node, curFunc int64, seen map[[2]int64]bool) {
	if curFunc < 0 {
		return
	}
	to, ok := w.funcIdx[w.calleeName(call)]
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

func (w *kotlinWalk) calleeName(call *tsg.Node) string {
	callee := firstNamedChild(call)
	if callee == nil {
		return ""
	}
	switch callee.Type(w.lang) {
	case "simple_identifier":
		return callee.Text(w.src)
	case "navigation_expression":
		if suf := childByType(callee, "navigation_suffix", w.lang); suf != nil {
			if id := childByType(suf, "simple_identifier", w.lang); id != nil {
				return id.Text(w.src)
			}
		}
	}
	return ""
}

// lastTypeIdentifier returns the deepest-rightmost type_identifier under n, so a
// qualified annotation like `@kotlin.test.Test` yields its simple name "Test".
func lastTypeIdentifier(n *tsg.Node, lang *tsg.Language, src []byte) string {
	var last string
	var rec func(*tsg.Node)
	rec = func(m *tsg.Node) {
		if m.Type(lang) == "type_identifier" {
			last = m.Text(src)
		}
		for _, c := range m.Children() {
			rec(c)
		}
	}
	rec(n)
	return last
}

func stripBackticks(s string) string { return strings.Trim(s, "`") }
