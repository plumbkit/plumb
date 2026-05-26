package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/golimpio/plumb/internal/topology"
)

// TypeScriptExtractor extracts TypeScript (.ts) symbols using the gotreesitter
// TypeScript grammar. The missing external lex-states table is supplied via
// registerTSLexStates (see ts_lex_states.go) so typed arrow parameters no longer
// cascade ERROR nodes — this replaces the legacy regex extractor for .ts.
//
// .tsx/.jsx are deliberately NOT handled here: even with the regenerated TSX
// lex-states, gotreesitter v0.19.1's TSX grammar still cascades on typed arrow
// parameters (a residual issue beyond the lex-states gap — JSX itself parses
// clean), which would silently drop trailing symbols. They stay on the regex
// extractor until the TSX grammar is fixed (or driven via WASM). See
// docs/internal/treesitter-plan.md.
//
// Concurrency: stateless after construction and safe for concurrent use; a
// fresh parser is created per Extract call because gotreesitter parsers are not
// safe for concurrent reuse.
type TypeScriptExtractor struct {
	ts *tsg.Language
}

// NewTypeScript returns a tree-sitter-backed TypeScript (.ts) extractor. It
// registers the TypeScript external lex-states before loading the grammar so
// the loader attaches them.
func NewTypeScript() *TypeScriptExtractor {
	registerTSLexStates()
	return &TypeScriptExtractor{ts: grammars.TypescriptLanguage()}
}

func (e *TypeScriptExtractor) Language() string     { return "typescript" }
func (e *TypeScriptExtractor) Extensions() []string { return []string{".ts"} }

// Extract parses src and returns top-level functions, classes with their
// methods, interfaces (→ KindType) with their method signatures, type aliases
// and enums (→ KindType), enum members (→ KindConstant), top-level
// constants/variables, imports, and describe/it/test blocks (→ KindTest);
// namespace bodies are descended into. Containment is lexical and certain
// (1.0/extractor); intra-file call edges are name-resolved heuristics (0.8).
// Returns (nil, nil, nil) when src cannot be parsed.
func (e *TypeScriptExtractor) Extract(_ context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	tree, err := tsg.NewParser(e.ts).Parse(src)
	if err != nil || tree == nil {
		return nil, nil, nil
	}
	w := &tsWalk{lang: e.ts, src: src, path: relPath, funcIdx: map[string]int64{}}
	for _, n := range tree.RootNode().Children() {
		w.dispatch(n)
	}
	w.scanTests(tree.RootNode())
	w.callEdges(tree.RootNode())
	return w.nodes, w.edges, nil
}

type tsWalk struct {
	lang    *tsg.Language
	src     []byte
	path    string
	nodes   []topology.Node
	edges   []topology.Edge
	funcIdx map[string]int64
}

// dispatch handles one top-level (or namespace-level / export-unwrapped) node.
func (w *tsWalk) dispatch(n *tsg.Node) {
	switch n.Type(w.lang) {
	case "function_declaration", "generator_function_declaration":
		w.addFunc(n, -1)
	case "class_declaration", "abstract_class_declaration":
		w.addClass(n)
	case "interface_declaration":
		w.addInterface(n)
	case "type_alias_declaration":
		w.addNamedType(n)
	case "enum_declaration":
		w.addEnum(n)
	case "lexical_declaration", "variable_declaration":
		w.handleVarDecl(n)
	case "import_statement":
		w.addImport(n)
	case "export_statement":
		w.walkExport(n)
	case "internal_module", "module":
		w.walkNamespace(n)
	case "expression_statement":
		// `namespace X {…}` parses as expression_statement > internal_module.
		if im := childByType(n, "internal_module", w.lang); im != nil {
			w.walkNamespace(im)
		}
	}
}

// walkExport unwraps `export`/`export default` and dispatches the inner declaration.
func (w *tsWalk) walkExport(n *tsg.Node) {
	for _, c := range n.Children() {
		w.dispatch(c)
	}
}

// walkNamespace descends into a namespace/module body so nested declarations are indexed.
func (w *tsWalk) walkNamespace(n *tsg.Node) {
	body := childByType(n, "statement_block", w.lang)
	if body == nil {
		return
	}
	for _, c := range body.Children() {
		w.dispatch(c)
	}
}

func (w *tsWalk) addFunc(n *tsg.Node, enclosing int64) {
	if name := w.nodeName(n); name != "" {
		w.appendFunc(name, n, enclosing)
	}
}

// appendFunc records a function/method node and registers it for call-edge
// resolution. A method (enclosing >= 0) also gains a containment edge.
func (w *tsWalk) appendFunc(name string, rng *tsg.Node, enclosing int64) {
	kind := topology.KindFunction
	if enclosing >= 0 {
		kind = topology.KindMethod
	}
	idx := w.appendNode(kind, name, rng)
	w.funcIdx[name] = idx
	if enclosing >= 0 {
		w.containment(enclosing, idx)
	}
}

func (w *tsWalk) addClass(n *tsg.Node) {
	name := w.nodeName(n)
	if name == "" {
		return
	}
	idx := w.appendNode(topology.KindClass, name, n)
	if body := childByType(n, "class_body", w.lang); body != nil {
		for _, m := range body.Children() {
			switch m.Type(w.lang) {
			case "method_definition":
				if mn := w.memberName(m); mn != "" {
					w.appendFunc(mn, m, idx)
				}
			case "public_field_definition":
				w.addField(m, idx)
			}
		}
	}
}

func (w *tsWalk) addInterface(n *tsg.Node) {
	name := w.nodeName(n)
	if name == "" {
		return
	}
	idx := w.appendNode(topology.KindType, name, n)
	if body := childByType(n, "interface_body", w.lang); body != nil {
		for _, m := range body.Children() {
			switch m.Type(w.lang) {
			case "method_signature":
				if mn := w.memberName(m); mn != "" {
					w.appendFunc(mn, m, idx)
				}
			case "property_signature":
				w.addField(m, idx)
			}
		}
	}
}

// addField records a class property or interface property signature as a member
// of its type: KindConstant when declared readonly, else KindVariable.
func (w *tsWalk) addField(m *tsg.Node, enclosing int64) {
	name := w.memberName(m)
	if name == "" {
		return
	}
	kind := topology.KindVariable
	if w.fieldReadonly(m) {
		kind = topology.KindConstant
	}
	w.containment(enclosing, w.appendNode(kind, name, m))
}

// fieldReadonly reports whether a `readonly` modifier precedes the member name.
func (w *tsWalk) fieldReadonly(m *tsg.Node) bool {
	name := childByType(m, "property_identifier", w.lang)
	if name == nil {
		return false
	}
	for _, tok := range strings.Fields(string(w.src[m.StartByte():name.StartByte()])) {
		if tok == "readonly" {
			return true
		}
	}
	return false
}

func (w *tsWalk) addNamedType(n *tsg.Node) {
	if name := w.typeName(n); name != "" {
		w.appendNode(topology.KindType, name, n)
	}
}

func (w *tsWalk) addEnum(n *tsg.Node) {
	name := w.nodeName(n)
	if name == "" {
		return
	}
	idx := w.appendNode(topology.KindType, name, n)
	body := childByType(n, "enum_body", w.lang)
	if body == nil {
		return
	}
	for _, m := range body.Children() {
		var member string
		switch m.Type(w.lang) {
		case "property_identifier":
			member = m.Text(w.src)
		case "enum_assignment":
			if id := childByType(m, "property_identifier", w.lang); id != nil {
				member = id.Text(w.src)
			}
		}
		if member != "" {
			w.containment(idx, w.appendNode(topology.KindConstant, member, m))
		}
	}
}

// handleVarDecl classifies a const/let/var declaration: an arrow/function
// initialiser is a function, a require(...) call is an import, otherwise a
// constant (const) or variable (let/var).
func (w *tsWalk) handleVarDecl(n *tsg.Node) {
	isConst := strings.HasPrefix(strings.TrimSpace(n.Text(w.src)), "const")
	for _, d := range n.Children() {
		if d.Type(w.lang) == "variable_declarator" {
			w.handleDeclarator(d, n, isConst)
		}
	}
}

func (w *tsWalk) handleDeclarator(d, decl *tsg.Node, isConst bool) {
	name := w.declaratorName(d)
	if name == "" {
		return
	}
	value := d.ChildByFieldName("value", w.lang)
	switch {
	case value != nil && jsFuncValues[value.Type(w.lang)]:
		w.appendFunc(name, decl, -1)
	case value != nil && w.isRequire(value):
		if target := w.callStringArg(value); target != "" {
			w.appendImport(target, decl)
		}
	case isConst:
		w.appendNode(topology.KindConstant, name, decl)
	default:
		w.appendNode(topology.KindVariable, name, decl)
	}
}

func (w *tsWalk) isRequire(value *tsg.Node) bool {
	if value.Type(w.lang) != "call_expression" {
		return false
	}
	fn := firstNamedChild(value)
	return fn != nil && fn.Type(w.lang) == "identifier" && fn.Text(w.src) == "require"
}

func (w *tsWalk) addImport(n *tsg.Node) {
	if str := childByType(n, "string", w.lang); str != nil {
		w.appendImport(w.stringText(str), n)
	}
}

func (w *tsWalk) appendImport(target string, rng *tsg.Node) {
	if target == "" {
		return
	}
	w.nodes = append(w.nodes, topology.Node{
		Kind:      topology.KindImport,
		Name:      target,
		Qualified: target,
		StartLine: line(rng.StartPoint()),
		Language:  "typescript",
		Path:      w.path,
	})
}

// appendNode records a node spanning rng and returns its index.
func (w *tsWalk) appendNode(kind topology.NodeKind, name string, rng *tsg.Node) int64 {
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		StartLine: line(rng.StartPoint()),
		EndLine:   line(rng.EndPoint()),
		Language:  "typescript",
		Path:      w.path,
	})
	return idx
}

func (w *tsWalk) containment(from, to int64) {
	w.edges = append(w.edges, topology.Edge{
		FromID:     from,
		ToID:       to,
		Kind:       topology.EdgeContains,
		Confidence: 1.0,
		Source:     "extractor",
	})
}

// nodeName returns the declaration's `name` field, else its first type/plain identifier.
func (w *tsWalk) nodeName(n *tsg.Node) string {
	if id := n.ChildByFieldName("name", w.lang); id != nil {
		return id.Text(w.src)
	}
	if id := childByType(n, "type_identifier", w.lang); id != nil {
		return id.Text(w.src)
	}
	if id := childByType(n, "identifier", w.lang); id != nil {
		return id.Text(w.src)
	}
	return ""
}

func (w *tsWalk) typeName(n *tsg.Node) string {
	if id := childByType(n, "type_identifier", w.lang); id != nil {
		return id.Text(w.src)
	}
	return ""
}

func (w *tsWalk) memberName(m *tsg.Node) string {
	if id := m.ChildByFieldName("name", w.lang); id != nil {
		return id.Text(w.src)
	}
	if id := childByType(m, "property_identifier", w.lang); id != nil {
		return id.Text(w.src)
	}
	return ""
}

func (w *tsWalk) declaratorName(d *tsg.Node) string {
	if id := d.ChildByFieldName("name", w.lang); id != nil && id.Type(w.lang) == "identifier" {
		return id.Text(w.src)
	}
	if id := childByType(d, "identifier", w.lang); id != nil {
		return id.Text(w.src)
	}
	return ""
}

func (w *tsWalk) callStringArg(call *tsg.Node) string {
	args := childByType(call, "arguments", w.lang)
	if args == nil {
		return ""
	}
	if str := childByType(args, "string", w.lang); str != nil {
		return w.stringText(str)
	}
	return ""
}

func (w *tsWalk) stringText(str *tsg.Node) string {
	if frag := childByType(str, "string_fragment", w.lang); frag != nil {
		return frag.Text(w.src)
	}
	return strings.Trim(str.Text(w.src), "\"'`")
}

// scanTests walks the whole tree emitting a KindTest node for every
// describe/it/test call at any nesting depth.
func (w *tsWalk) scanTests(root *tsg.Node) {
	var rec func(*tsg.Node)
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

func (w *tsWalk) maybeTest(call *tsg.Node) {
	fn := firstNamedChild(call)
	if fn == nil || fn.Type(w.lang) != "identifier" || !jsTestFns[fn.Text(w.src)] {
		return
	}
	name := w.callStringArg(call)
	if name == "" {
		name = fn.Text(w.src)
	}
	w.nodes = append(w.nodes, topology.Node{
		Kind:      topology.KindTest,
		Name:      name,
		Qualified: name,
		StartLine: line(call.StartPoint()),
		EndLine:   line(call.EndPoint()),
		Language:  "typescript",
		Path:      w.path,
	})
}

// callEdges emits EdgeCalls between functions defined in the file (0.8/heuristic).
func (w *tsWalk) callEdges(root *tsg.Node) {
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

func (w *tsWalk) enclosingFunc(n *tsg.Node, curFunc int64) int64 {
	var name string
	switch n.Type(w.lang) {
	case "function_declaration", "generator_function_declaration":
		name = w.nodeName(n)
	case "method_definition":
		name = w.memberName(n)
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

func (w *tsWalk) maybeCallEdge(call *tsg.Node, curFunc int64, seen map[[2]int64]bool) {
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

func (w *tsWalk) calleeName(callee *tsg.Node) string {
	switch callee.Type(w.lang) {
	case "identifier":
		return callee.Text(w.src)
	case "member_expression":
		if prop := callee.ChildByFieldName("property", w.lang); prop != nil {
			return prop.Text(w.src)
		}
	}
	return ""
}
