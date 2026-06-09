package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/golimpio/plumb/internal/topology"
)

// ZigExtractor extracts Zig symbols using the gotreesitter Zig grammar.
//
// Concurrency: stateless after construction and safe for concurrent use; a
// fresh parser is created per Extract call because gotreesitter parsers are not
// safe for concurrent reuse.
type ZigExtractor struct {
	lang lazyGrammar
}

// NewZig returns a tree-sitter-backed Zig extractor.
func NewZig() *ZigExtractor {
	return &ZigExtractor{lang: lazyGrammar{load: grammars.ZigLanguage}}
}

func (e *ZigExtractor) Language() string     { return "zig" }
func (e *ZigExtractor) Extensions() []string { return []string{".zig"} }

// Extract parses src and returns Zig functions, container methods, types
// (struct/enum/union bound to a const), constants, variables, @import imports
// and tests, plus container → method containment edges and intra-file call
// edges. Containment is lexical (the method is inside the container literal) and
// therefore certain (1.0); intra-file call edges are name-resolved heuristics
// (0.8). Returns (nil, nil, nil) when src cannot be parsed.
func (e *ZigExtractor) Extract(_ context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	tree, err := tsg.NewParser(e.lang.get()).Parse(src)
	if err != nil || tree == nil {
		return nil, nil, nil
	}
	defer tree.Release()
	w := &zigWalk{lang: e.lang.get(), src: src, path: relPath, funcIdx: map[string]int64{}}
	w.walk(tree.RootNode(), -1, false)
	w.callEdges(tree.RootNode())
	return w.nodes, w.edges, nil
}

type zigWalk struct {
	lang    *tsg.Language
	src     []byte
	path    string
	nodes   []topology.Node
	edges   []topology.Edge
	funcIdx map[string]int64 // function/method name → node index, for call edges
}

// containerTypes are the value node types that make a `const X = <value>`
// declaration a named type rather than a plain binding.
var zigContainerTypes = map[string]bool{
	"struct_declaration":    true,
	"enum_declaration":      true,
	"union_declaration":     true,
	"opaque_declaration":    true,
	"error_set_declaration": true,
}

func (w *zigWalk) walk(n *tsg.Node, enclosingType int64, inFunc bool) {
	switch n.Type(w.lang) {
	case "variable_declaration":
		// const/var declared inside a function body are locals — not surfaced.
		if !inFunc {
			w.handleVarDecl(n, enclosingType)
		}
	case "function_declaration":
		w.addFunc(n, enclosingType)
		w.walkChildren(n, -1, true)
	case "test_declaration":
		w.addTest(n)
	default:
		w.walkChildren(n, enclosingType, inFunc)
	}
}

func (w *zigWalk) walkChildren(n *tsg.Node, enclosingType int64, inFunc bool) {
	for _, c := range n.Children() {
		w.walk(c, enclosingType, inFunc)
	}
}

// handleVarDecl classifies a `const`/`var` declaration by its value: a struct/
// enum/union literal becomes a type (whose nested functions are methods), an
// @import becomes an import, anything else a constant or variable.
func (w *zigWalk) handleVarDecl(n *tsg.Node, enclosingType int64) {
	name := w.declName(n)
	if name == "" {
		w.walkChildren(n, enclosingType, false)
		return
	}
	value := w.declValue(n)
	if value != nil {
		switch {
		case zigContainerTypes[value.Type(w.lang)]:
			idx := w.addType(n, name)
			w.addContainerFields(value, idx, value.Type(w.lang) == "enum_declaration")
			w.walkChildren(value, idx, false)
			return
		case value.Type(w.lang) == "builtin_function" && w.isImport(value):
			w.addImport(value, n)
			return
		}
	}
	kind := topology.KindVariable
	if w.isConst(n) {
		kind = topology.KindConstant
	}
	w.addBinding(n, name, kind)
}

// declName returns the declared identifier — the first identifier child, which
// in Zig's grammar precedes any type annotation and the value.
func (w *zigWalk) declName(n *tsg.Node) string {
	if id := childByType(n, "identifier", w.lang); id != nil {
		return id.Text(w.src)
	}
	return ""
}

// declValue returns the initialiser — the last named child of the declaration.
func (w *zigWalk) declValue(n *tsg.Node) *tsg.Node {
	var last *tsg.Node
	for _, c := range n.Children() {
		if c.IsNamed() {
			last = c
		}
	}
	return last
}

func (w *zigWalk) isConst(n *tsg.Node) bool {
	t := strings.TrimSpace(n.Text(w.src))
	for _, p := range []string{"pub ", "export ", "extern ", "threadlocal ", "comptime "} {
		t = strings.TrimPrefix(t, p)
	}
	return strings.HasPrefix(t, "const")
}

func (w *zigWalk) isImport(bf *tsg.Node) bool {
	if id := childByType(bf, "builtin_identifier", w.lang); id != nil {
		return id.Text(w.src) == "@import"
	}
	return false
}

func (w *zigWalk) addType(n *tsg.Node, name string) int64 {
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      topology.KindType,
		Name:      name,
		Qualified: name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "zig",
		Path:      w.path,
	})
	return idx
}

// addContainerFields records the members of a container literal: struct/union
// fields become variables, enum members become constants — each contained in
// the type (1.0/extractor). Methods (function_declaration) are added separately
// by the generic walk, so this only handles container_field nodes.
func (w *zigWalk) addContainerFields(container *tsg.Node, typeIdx int64, isEnum bool) {
	if typeIdx < 0 {
		return
	}
	kind := topology.KindVariable
	if isEnum {
		kind = topology.KindConstant
	}
	for _, c := range container.Children() {
		if c.Type(w.lang) != "container_field" {
			continue
		}
		id := childByType(c, "identifier", w.lang)
		if id == nil {
			continue
		}
		w.addMember(c, kind, id.Text(w.src), typeIdx)
	}
}

// addMember appends a member node and a certain (1.0/extractor) containment
// edge to its enclosing type.
func (w *zigWalk) addMember(n *tsg.Node, kind topology.NodeKind, name string, typeIdx int64) {
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "zig",
		Path:      w.path,
	})
	w.edges = append(w.edges, topology.Edge{
		FromID:     typeIdx,
		ToID:       idx,
		Kind:       topology.EdgeContains,
		Confidence: 1.0,
		Source:     "extractor",
	})
}

func (w *zigWalk) addFunc(n *tsg.Node, enclosingType int64) {
	name := ""
	if id := n.ChildByFieldName("name", w.lang); id != nil {
		name = id.Text(w.src)
	}
	if name == "" {
		return
	}
	kind := topology.KindFunction
	if enclosingType >= 0 {
		kind = topology.KindMethod
	}
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "zig",
		Path:      w.path,
	})
	w.funcIdx[name] = idx
	if enclosingType >= 0 {
		w.edges = append(w.edges, topology.Edge{
			FromID:     enclosingType,
			ToID:       idx,
			Kind:       topology.EdgeContains,
			Confidence: 1.0,
			Source:     "extractor",
		})
	}
}

func (w *zigWalk) addBinding(n *tsg.Node, name string, kind topology.NodeKind) {
	w.nodes = append(w.nodes, topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "zig",
		Path:      w.path,
	})
}

func (w *zigWalk) addImport(bf *tsg.Node, decl *tsg.Node) {
	name := w.importTarget(bf)
	if name == "" {
		return
	}
	w.nodes = append(w.nodes, topology.Node{
		Kind:      topology.KindImport,
		Name:      name,
		StartLine: line(decl.StartPoint()),
		Language:  "zig",
		Path:      w.path,
	})
}

func (w *zigWalk) importTarget(bf *tsg.Node) string {
	args := childByType(bf, "arguments", w.lang)
	if args == nil {
		return ""
	}
	str := childByType(args, "string", w.lang)
	if str == nil {
		return ""
	}
	return strings.Trim(str.Text(w.src), `"`)
}

func (w *zigWalk) addTest(n *tsg.Node) {
	name := "test"
	if str := childByType(n, "string", w.lang); str != nil {
		name = strings.Trim(str.Text(w.src), `"`)
	}
	w.nodes = append(w.nodes, topology.Node{
		Kind:      topology.KindTest,
		Name:      name,
		Qualified: name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "zig",
		Path:      w.path,
	})
}

// callEdges does a second pass emitting EdgeCalls between functions defined in
// the file. The call site is syntactically certain but the callee is resolved
// by name within the file, so confidence is 0.8 (heuristic).
func (w *zigWalk) callEdges(root *tsg.Node) {
	seen := map[[2]int64]bool{}
	var rec func(n *tsg.Node, curFunc int64)
	rec = func(n *tsg.Node, curFunc int64) {
		switch n.Type(w.lang) {
		case "function_declaration":
			if id := n.ChildByFieldName("name", w.lang); id != nil {
				if idx, ok := w.funcIdx[id.Text(w.src)]; ok {
					curFunc = idx
				}
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

func (w *zigWalk) maybeCallEdge(call *tsg.Node, curFunc int64, seen map[[2]int64]bool) {
	if curFunc < 0 {
		return
	}
	callee := firstNamedChild(call)
	if callee == nil {
		return
	}
	to, ok := w.funcIdx[w.calleeName(callee)]
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

func (w *zigWalk) calleeName(callee *tsg.Node) string {
	switch callee.Type(w.lang) {
	case "identifier":
		return callee.Text(w.src)
	case "field_expression":
		return w.lastIdentifier(callee)
	}
	return ""
}

func (w *zigWalk) lastIdentifier(n *tsg.Node) string {
	var last string
	for _, c := range n.Children() {
		if t := c.Type(w.lang); t == "identifier" || t == "field_identifier" {
			last = c.Text(w.src)
		}
	}
	return last
}
