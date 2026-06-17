package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// JavaScriptExtractor extracts JavaScript symbols using the gotreesitter
// JavaScript grammar. Plain JavaScript (.js/.mjs/.cjs) parses cleanly — unlike
// TypeScript, it has no typed-arrow syntax to trip the missing external
// lex-states table — so it is split off the regex TS/JS extractor.
//
// Concurrency: stateless after construction and safe for concurrent use; a
// fresh parser is created per Extract call because gotreesitter parsers are not
// safe for concurrent reuse.
type JavaScriptExtractor struct {
	lang lazyGrammar
}

// NewJavaScript returns a tree-sitter-backed JavaScript extractor.
func NewJavaScript() *JavaScriptExtractor {
	return &JavaScriptExtractor{lang: lazyGrammar{load: grammars.JavascriptLanguage}}
}

func (e *JavaScriptExtractor) Language() string     { return "javascript" }
func (e *JavaScriptExtractor) Extensions() []string { return []string{".js", ".mjs", ".cjs"} }

// Extract parses src and returns top-level functions (declarations and
// arrow/function-expression bindings), classes with their methods, top-level
// constants/variables, ES `import` and CommonJS `require` imports, and
// describe/it/test blocks (→ KindTest). Class → method containment is lexical
// and therefore certain (1.0); intra-file call edges are name-resolved
// heuristics (0.8). Returns (nil, nil, nil) when src cannot be parsed.
func (e *JavaScriptExtractor) Extract(_ context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	tree, err := tsg.NewParser(e.lang.get()).Parse(src)
	if err != nil || tree == nil {
		return nil, nil, nil
	}
	defer tree.Release()
	w := &jsWalk{lang: e.lang.get(), src: src, path: relPath, funcIdx: map[string]int64{}}
	w.walk(tree.RootNode())
	w.scanTests(tree.RootNode())
	w.callEdges(tree.RootNode())
	return w.nodes, w.edges, nil
}

type jsWalk struct {
	lang    *tsg.Language
	src     []byte
	path    string
	nodes   []topology.Node
	edges   []topology.Edge
	funcIdx map[string]int64 // function/method name → node index, for call edges
}

// walk scans the program's direct children. Declarations are top-level by
// convention, so a flat pass avoids surfacing function-local bindings as
// module-level symbols (tests, nested at any depth, are found by scanTests).
func (w *jsWalk) walk(root *tsg.Node) {
	for _, n := range root.Children() {
		switch n.Type(w.lang) {
		case "function_declaration", "generator_function_declaration":
			w.addFunc(n, -1)
		case "class_declaration":
			w.addClass(n)
		case "lexical_declaration", "variable_declaration":
			w.handleVarDecl(n)
		case "import_statement":
			w.addImport(n)
		case "export_statement":
			w.handleExport(n)
		}
	}
}

// handleExport unwraps `export`/`export default` declarations and dispatches the
// inner declaration, so exported functions/classes/bindings are still indexed.
func (w *jsWalk) handleExport(n *tsg.Node) {
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "function_declaration", "generator_function_declaration":
			w.addFunc(c, -1)
		case "class_declaration":
			w.addClass(c)
		case "lexical_declaration", "variable_declaration":
			w.handleVarDecl(c)
		}
	}
}

func (w *jsWalk) addFunc(n *tsg.Node, enclosingType int64) {
	name := w.nodeName(n)
	if name == "" {
		return
	}
	w.appendFunc(name, n, enclosingType)
}

// appendFunc records a function/method node and registers it for call-edge
// resolution. A method (enclosingType >= 0) also gains a containment edge.
func (w *jsWalk) appendFunc(name string, rng *tsg.Node, enclosingType int64) {
	kind := topology.KindFunction
	if enclosingType >= 0 {
		kind = topology.KindMethod
	}
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		StartLine: line(rng.StartPoint()),
		EndLine:   line(rng.EndPoint()),
		Language:  "javascript",
		Path:      w.path,
	}
	setSpan(&node, rng)
	node.DocStartByte, node.DocEndByte = docSpanBefore(rng, w.lang, jsIsComment)
	w.nodes = append(w.nodes, node)
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

func (w *jsWalk) addClass(n *tsg.Node) {
	name := w.nodeName(n)
	if name == "" {
		return
	}
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      topology.KindClass,
		Name:      name,
		Qualified: name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "javascript",
		Path:      w.path,
	}
	setSpan(&node, n)
	node.DocStartByte, node.DocEndByte = docSpanBefore(n, w.lang, jsIsComment)
	w.nodes = append(w.nodes, node)
	if body := childByType(n, "class_body", w.lang); body != nil {
		w.addClassMembers(body, idx)
	}
}

// addClassMembers records a class body's methods (as methods) and field
// definitions (as variables — plain JS has no immutability marker), each
// contained in the class (1.0/extractor).
func (w *jsWalk) addClassMembers(body *tsg.Node, classIdx int64) {
	for _, m := range body.Children() {
		switch m.Type(w.lang) {
		case "method_definition":
			if name := w.methodName(m); name != "" {
				w.appendFunc(name, m, classIdx)
			}
		case "field_definition":
			if name := w.fieldDefName(m); name != "" {
				w.addMember(m, classIdx, name)
			}
		}
	}
}

// fieldDefName returns a class field's name (public or #private).
func (w *jsWalk) fieldDefName(m *tsg.Node) string {
	if id := m.ChildByFieldName("name", w.lang); id != nil {
		return id.Text(w.src)
	}
	for _, typ := range []string{"property_identifier", "private_property_identifier"} {
		if id := childByType(m, typ, w.lang); id != nil {
			return id.Text(w.src)
		}
	}
	return ""
}

// addMember appends a class field (KindVariable) and a certain (1.0/extractor)
// containment edge to its class.
func (w *jsWalk) addMember(rng *tsg.Node, classIdx int64, name string) {
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      topology.KindVariable,
		Name:      name,
		Qualified: name,
		StartLine: line(rng.StartPoint()),
		EndLine:   line(rng.EndPoint()),
		Language:  "javascript",
		Path:      w.path,
	}
	setSpan(&node, rng)
	w.nodes = append(w.nodes, node)
	w.edges = append(w.edges, topology.Edge{
		FromID:     classIdx,
		ToID:       idx,
		Kind:       topology.EdgeContains,
		Confidence: 1.0,
		Source:     "extractor",
	})
}

func (w *jsWalk) methodName(m *tsg.Node) string {
	if id := m.ChildByFieldName("name", w.lang); id != nil {
		return id.Text(w.src)
	}
	if id := childByType(m, "property_identifier", w.lang); id != nil {
		return id.Text(w.src)
	}
	return ""
}

// handleVarDecl classifies a `const`/`let`/`var` declaration by its initialiser:
// an arrow/function expression is a function, a `require(...)` call is an import,
// anything else a constant (const) or variable (let/var).
func (w *jsWalk) handleVarDecl(n *tsg.Node) {
	isConst := strings.HasPrefix(strings.TrimSpace(n.Text(w.src)), "const")
	for _, d := range n.Children() {
		if d.Type(w.lang) != "variable_declarator" {
			continue
		}
		w.handleDeclarator(d, n, isConst)
	}
}

func (w *jsWalk) handleDeclarator(d, decl *tsg.Node, isConst bool) {
	name := w.declaratorName(d)
	if name == "" {
		return
	}
	value := d.ChildByFieldName("value", w.lang)
	switch {
	case value != nil && jsFuncValues[value.Type(w.lang)]:
		w.appendFunc(name, decl, -1)
	case value != nil && w.isRequire(value):
		w.addRequireImport(value, decl)
	case isConst:
		w.addBinding(name, decl, topology.KindConstant)
	default:
		w.addBinding(name, decl, topology.KindVariable)
	}
}

// jsFuncValues are the initialiser node types that make a binding a function.
var jsFuncValues = map[string]bool{
	"arrow_function":      true,
	"function_expression": true,
	"function":            true,
	"generator_function":  true,
}

func (w *jsWalk) declaratorName(d *tsg.Node) string {
	if id := d.ChildByFieldName("name", w.lang); id != nil && id.Type(w.lang) == "identifier" {
		return id.Text(w.src)
	}
	if id := childByType(d, "identifier", w.lang); id != nil {
		return id.Text(w.src)
	}
	return ""
}

func (w *jsWalk) addBinding(name string, rng *tsg.Node, kind topology.NodeKind) {
	node := topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		StartLine: line(rng.StartPoint()),
		EndLine:   line(rng.EndPoint()),
		Language:  "javascript",
		Path:      w.path,
	}
	setSpan(&node, rng)
	w.nodes = append(w.nodes, node)
}

// isRequire reports whether a call expression is a CommonJS `require(...)` call.
func (w *jsWalk) isRequire(value *tsg.Node) bool {
	if value.Type(w.lang) != "call_expression" {
		return false
	}
	fn := firstNamedChild(value)
	return fn != nil && fn.Type(w.lang) == "identifier" && fn.Text(w.src) == "require"
}

func (w *jsWalk) addRequireImport(call, decl *tsg.Node) {
	if target := w.callStringArg(call); target != "" {
		w.appendImport(target, decl)
	}
}

func (w *jsWalk) addImport(n *tsg.Node) {
	if str := childByType(n, "string", w.lang); str != nil {
		w.appendImport(w.stringText(str), n)
	}
}

func (w *jsWalk) appendImport(target string, rng *tsg.Node) {
	if target == "" {
		return
	}
	node := topology.Node{
		Kind:      topology.KindImport,
		Name:      target,
		Qualified: target,
		StartLine: line(rng.StartPoint()),
		Language:  "javascript",
		Path:      w.path,
	}
	setSpan(&node, rng)
	w.nodes = append(w.nodes, node)
}

// callStringArg returns the first string argument of a call expression, stripped
// of its quotes — the module path of a `require('...')` call.
func (w *jsWalk) callStringArg(call *tsg.Node) string {
	args := childByType(call, "arguments", w.lang)
	if args == nil {
		return ""
	}
	if str := childByType(args, "string", w.lang); str != nil {
		return w.stringText(str)
	}
	return ""
}

func (w *jsWalk) stringText(str *tsg.Node) string {
	if frag := childByType(str, "string_fragment", w.lang); frag != nil {
		return frag.Text(w.src)
	}
	return strings.Trim(str.Text(w.src), "\"'`")
}

func (w *jsWalk) nodeName(n *tsg.Node) string {
	if id := n.ChildByFieldName("name", w.lang); id != nil {
		return id.Text(w.src)
	}
	if id := childByType(n, "identifier", w.lang); id != nil {
		return id.Text(w.src)
	}
	return ""
}

// jsTestFns are the global test-block functions recognised as KindTest.
var jsTestFns = map[string]bool{"describe": true, "it": true, "test": true}

// scanTests walks the whole tree emitting a KindTest node for every
// describe/it/test call, at any nesting depth (it/test live inside describe's
// callback).
func (w *jsWalk) scanTests(root *tsg.Node) {
	var rec func(n *tsg.Node)
	rec = func(n *tsg.Node) {
		if n.Type(w.lang) == "call_expression" {
			w.maybeTest(n)
		}
		for _, c := range n.Children() {
			rec(c)
		}
	}
	rec(root)
}

func (w *jsWalk) maybeTest(call *tsg.Node) {
	fn := firstNamedChild(call)
	if fn == nil || fn.Type(w.lang) != "identifier" || !jsTestFns[fn.Text(w.src)] {
		return
	}
	name := w.callStringArg(call)
	if name == "" {
		name = fn.Text(w.src)
	}
	node := topology.Node{
		Kind:      topology.KindTest,
		Name:      name,
		Qualified: name,
		StartLine: line(call.StartPoint()),
		EndLine:   line(call.EndPoint()),
		Language:  "javascript",
		Path:      w.path,
	}
	setSpan(&node, call)
	w.nodes = append(w.nodes, node)
}

// jsIsComment reports whether a JavaScript grammar node type is a comment.
func jsIsComment(typ string) bool { return typ == "comment" }

// callEdges does a second pass emitting EdgeCalls between functions defined in
// the file. The call site is syntactically certain but the callee is resolved
// by name within the file, so confidence is 0.8 (heuristic).
func (w *jsWalk) callEdges(root *tsg.Node) {
	seen := map[[2]int64]bool{}
	var rec func(n *tsg.Node, curFunc int64)
	rec = func(n *tsg.Node, curFunc int64) {
		curFunc = w.enclosingFunc(n, curFunc)
		if n.Type(w.lang) == "call_expression" {
			w.maybeCallEdge(n, curFunc, seen)
		}
		for _, c := range n.Children() {
			rec(c, curFunc)
		}
	}
	rec(root, -1)
}

// enclosingFunc updates the current enclosing-function index when entering a
// node that names a registered function (declaration, method, or arrow/function
// binding).
func (w *jsWalk) enclosingFunc(n *tsg.Node, curFunc int64) int64 {
	var name string
	switch n.Type(w.lang) {
	case "function_declaration", "generator_function_declaration":
		name = w.nodeName(n)
	case "method_definition":
		name = w.methodName(n)
	case "variable_declarator":
		if v := n.ChildByFieldName("value", w.lang); v != nil && jsFuncValues[v.Type(w.lang)] {
			name = w.declaratorName(n)
		}
	}
	if name == "" {
		return curFunc
	}
	if idx, ok := w.funcIdx[name]; ok {
		return idx
	}
	return curFunc
}

func (w *jsWalk) maybeCallEdge(call *tsg.Node, curFunc int64, seen map[[2]int64]bool) {
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

// calleeName resolves the called name from a call expression's callee: a bare
// identifier directly, or the trailing property of a member expression
// (`obj.method()` → "method").
func (w *jsWalk) calleeName(callee *tsg.Node) string {
	switch callee.Type(w.lang) {
	case "identifier":
		return callee.Text(w.src)
	case "member_expression":
		if prop := callee.ChildByFieldName("property", w.lang); prop != nil {
			return prop.Text(w.src)
		}
		return w.lastIdentifier(callee)
	}
	return ""
}

func (w *jsWalk) lastIdentifier(n *tsg.Node) string {
	var last string
	for _, c := range n.Children() {
		if t := c.Type(w.lang); t == "identifier" || t == "property_identifier" {
			last = c.Text(w.src)
		}
	}
	return last
}
