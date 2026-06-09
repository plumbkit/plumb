package wasmts

import (
	"strings"

	"github.com/plumbkit/plumb/internal/topology"
)

// jsFuncValues are initialiser node types whose binding is treated as a function.
var jsFuncValues = map[string]bool{
	"arrow_function":      true,
	"function_expression": true,
	"function":            true,
	"generator_function":  true,
}

// jsTestFns are the call identifiers that introduce a test block.
var jsTestFns = map[string]bool{"describe": true, "it": true, "test": true}

// walk accumulates topology nodes/edges from one parsed TS/TSX/JSX tree. It is
// the canonical-grammar port of the gotreesitter TypeScript walk: node-type
// names are identical (both mirror tree-sitter-typescript), only the node
// accessors differ. Single-use, not concurrency-safe.
type walk struct {
	src     []byte
	path    string
	lang    string // node Language label: "typescript" or "tsx"
	lines   *lineMap
	nodes   []topology.Node
	edges   []topology.Edge
	funcIdx map[string]int64
}

// dispatch handles one top-level (or namespace-level / export-unwrapped) node.
func (w *walk) dispatch(n node) {
	switch n.kind() {
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
		if im := n.childByType("internal_module"); !im.isNull() {
			w.walkNamespace(im)
		}
	}
}

// walkExport unwraps `export`/`export default` and dispatches the inner declaration.
func (w *walk) walkExport(n node) {
	for _, c := range n.children() {
		w.dispatch(c)
	}
}

// walkNamespace descends into a namespace/module body so nested declarations are indexed.
func (w *walk) walkNamespace(n node) {
	body := n.childByType("statement_block")
	if body.isNull() {
		return
	}
	for _, c := range body.children() {
		w.dispatch(c)
	}
}

func (w *walk) addFunc(n node, enclosing int64) {
	if name := w.nodeName(n); name != "" {
		w.appendFunc(name, n, enclosing)
	}
}

// appendFunc records a function/method node and registers it for call-edge
// resolution. A method (enclosing >= 0) also gains a containment edge.
func (w *walk) appendFunc(name string, rng node, enclosing int64) {
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

func (w *walk) addClass(n node) {
	name := w.nodeName(n)
	if name == "" {
		return
	}
	idx := w.appendNode(topology.KindClass, name, n)
	body := n.childByType("class_body")
	if body.isNull() {
		return
	}
	for _, m := range body.children() {
		switch m.kind() {
		case "method_definition":
			if mn := w.memberName(m); mn != "" {
				w.appendFunc(mn, m, idx)
			}
		case "public_field_definition":
			w.addField(m, idx)
		}
	}
}

func (w *walk) addInterface(n node) {
	name := w.nodeName(n)
	if name == "" {
		return
	}
	idx := w.appendNode(topology.KindType, name, n)
	body := n.childByType("interface_body")
	if body.isNull() {
		return
	}
	for _, m := range body.children() {
		switch m.kind() {
		case "method_signature":
			if mn := w.memberName(m); mn != "" {
				w.appendFunc(mn, m, idx)
			}
		case "property_signature":
			w.addField(m, idx)
		}
	}
}

// addField records a class property or interface property signature as a member
// of its type: KindConstant when declared readonly, else KindVariable.
func (w *walk) addField(m node, enclosing int64) {
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
func (w *walk) fieldReadonly(m node) bool {
	name := m.childByType("property_identifier")
	if name.isNull() {
		return false
	}
	s, e := m.startByte(), name.startByte()
	if s < 0 || e > len(w.src) || s > e {
		return false
	}
	for _, tok := range strings.Fields(string(w.src[s:e])) {
		if tok == "readonly" {
			return true
		}
	}
	return false
}

func (w *walk) addNamedType(n node) {
	if name := w.typeName(n); name != "" {
		w.appendNode(topology.KindType, name, n)
	}
}

func (w *walk) addEnum(n node) {
	name := w.nodeName(n)
	if name == "" {
		return
	}
	idx := w.appendNode(topology.KindType, name, n)
	body := n.childByType("enum_body")
	if body.isNull() {
		return
	}
	for _, m := range body.children() {
		var member string
		switch m.kind() {
		case "property_identifier":
			member = m.text(w.src)
		case "enum_assignment":
			if id := m.childByType("property_identifier"); !id.isNull() {
				member = id.text(w.src)
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
func (w *walk) handleVarDecl(n node) {
	isConst := strings.HasPrefix(strings.TrimSpace(n.text(w.src)), "const")
	for _, d := range n.children() {
		if d.kind() == "variable_declarator" {
			w.handleDeclarator(d, n, isConst)
		}
	}
}

func (w *walk) handleDeclarator(d, decl node, isConst bool) {
	name := w.declaratorName(d)
	if name == "" {
		return
	}
	value := d.childByFieldName("value")
	switch {
	case !value.isNull() && jsFuncValues[value.kind()]:
		w.appendFunc(name, decl, -1)
	case !value.isNull() && w.isRequire(value):
		if target := w.callStringArg(value); target != "" {
			w.appendImport(target, decl)
		}
	case isConst:
		w.appendNode(topology.KindConstant, name, decl)
	default:
		w.appendNode(topology.KindVariable, name, decl)
	}
}

func (w *walk) isRequire(value node) bool {
	if value.kind() != "call_expression" {
		return false
	}
	fn := value.firstNamedChild()
	return !fn.isNull() && fn.kind() == "identifier" && fn.text(w.src) == "require"
}

func (w *walk) addImport(n node) {
	if str := n.childByType("string"); !str.isNull() {
		w.appendImport(w.stringText(str), n)
	}
}

func (w *walk) appendImport(target string, rng node) {
	if target == "" {
		return
	}
	w.nodes = append(w.nodes, topology.Node{
		Kind:      topology.KindImport,
		Name:      target,
		Qualified: target,
		StartLine: w.lines.at(rng.startByte()),
		Language:  w.lang,
		Path:      w.path,
	})
}

// appendNode records a node spanning rng and returns its index.
func (w *walk) appendNode(kind topology.NodeKind, name string, rng node) int64 {
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		StartLine: w.lines.at(rng.startByte()),
		EndLine:   w.lines.at(rng.endByte()),
		Language:  w.lang,
		Path:      w.path,
	})
	return idx
}

func (w *walk) containment(from, to int64) {
	w.edges = append(w.edges, topology.Edge{
		FromID:     from,
		ToID:       to,
		Kind:       topology.EdgeContains,
		Confidence: 1.0,
		Source:     "extractor",
	})
}

// nodeName returns the declaration's `name` field, else its first type/plain identifier.
func (w *walk) nodeName(n node) string {
	if id := n.childByFieldName("name"); !id.isNull() {
		return id.text(w.src)
	}
	if id := n.childByType("type_identifier"); !id.isNull() {
		return id.text(w.src)
	}
	if id := n.childByType("identifier"); !id.isNull() {
		return id.text(w.src)
	}
	return ""
}

func (w *walk) typeName(n node) string {
	if id := n.childByType("type_identifier"); !id.isNull() {
		return id.text(w.src)
	}
	return ""
}

func (w *walk) memberName(m node) string {
	if id := m.childByFieldName("name"); !id.isNull() {
		return id.text(w.src)
	}
	if id := m.childByType("property_identifier"); !id.isNull() {
		return id.text(w.src)
	}
	return ""
}

func (w *walk) declaratorName(d node) string {
	if id := d.childByFieldName("name"); !id.isNull() && id.kind() == "identifier" {
		return id.text(w.src)
	}
	if id := d.childByType("identifier"); !id.isNull() {
		return id.text(w.src)
	}
	return ""
}

func (w *walk) callStringArg(call node) string {
	args := call.childByType("arguments")
	if args.isNull() {
		return ""
	}
	if str := args.childByType("string"); !str.isNull() {
		return w.stringText(str)
	}
	return ""
}

func (w *walk) stringText(str node) string {
	if frag := str.childByType("string_fragment"); !frag.isNull() {
		return frag.text(w.src)
	}
	return strings.Trim(str.text(w.src), "\"'`")
}

// scanTests walks the whole tree emitting a KindTest node for every
// describe/it/test call at any nesting depth.
func (w *walk) scanTests(root node) {
	var rec func(node)
	rec = func(n node) {
		if n.kind() == "call_expression" {
			w.maybeTest(n)
		}
		for _, c := range n.children() {
			rec(c)
		}
	}
	rec(root)
}

func (w *walk) maybeTest(call node) {
	fn := call.firstNamedChild()
	if fn.isNull() || fn.kind() != "identifier" || !jsTestFns[fn.text(w.src)] {
		return
	}
	name := w.callStringArg(call)
	if name == "" {
		name = fn.text(w.src)
	}
	w.nodes = append(w.nodes, topology.Node{
		Kind:      topology.KindTest,
		Name:      name,
		Qualified: name,
		StartLine: w.lines.at(call.startByte()),
		EndLine:   w.lines.at(call.endByte()),
		Language:  w.lang,
		Path:      w.path,
	})
}

// callEdges emits EdgeCalls between functions defined in the file (0.8/heuristic).
func (w *walk) callEdges(root node) {
	seen := map[[2]int64]bool{}
	var rec func(n node, curFunc int64)
	rec = func(n node, curFunc int64) {
		curFunc = w.enclosingFunc(n, curFunc)
		if n.kind() == "call_expression" {
			w.maybeCallEdge(n, curFunc, seen)
		}
		for _, c := range n.children() {
			rec(c, curFunc)
		}
	}
	rec(root, -1)
}

func (w *walk) enclosingFunc(n node, curFunc int64) int64 {
	var name string
	switch n.kind() {
	case "function_declaration", "generator_function_declaration":
		name = w.nodeName(n)
	case "method_definition":
		name = w.memberName(n)
	case "variable_declarator":
		if v := n.childByFieldName("value"); !v.isNull() && jsFuncValues[v.kind()] {
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

func (w *walk) maybeCallEdge(call node, curFunc int64, seen map[[2]int64]bool) {
	if curFunc < 0 {
		return
	}
	callee := call.firstNamedChild()
	if callee.isNull() {
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

func (w *walk) calleeName(callee node) string {
	switch callee.kind() {
	case "identifier":
		return callee.text(w.src)
	case "member_expression":
		if prop := callee.childByFieldName("property"); !prop.isNull() {
			return prop.text(w.src)
		}
	}
	return ""
}
