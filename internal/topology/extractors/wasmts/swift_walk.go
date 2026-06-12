package wasmts

import (
	"strings"

	"github.com/plumbkit/plumb/internal/topology"
)

// swiftWalk accumulates topology nodes/edges from one parsed Swift tree. It is
// the canonical-grammar (alex-pinkus/tree-sitter-swift) counterpart of the
// gotreesitter Swift walk: it produces the same node kinds, signatures and
// edges, but maps the canonical node types/fields (e.g. a type's `name`/`body`
// fields, a property's `pattern bound_identifier`). Single-use; not
// concurrency-safe.
type swiftWalk struct {
	src     []byte
	path    string
	lines   *lineMap
	nodes   []topology.Node
	edges   []topology.Edge
	funcIdx map[string]int64 // function/method/test name → node index, for call edges
	conf    map[int64]string // type node index → conformance list text, for method signatures
}

func (w *swiftWalk) line(byteOff int) int { return w.lines.at(byteOff) }

// walk descends the tree. enclosing is the node index of the lexically enclosing
// type (-1 at file scope); inFunc is true once inside a function body
// (suppresses local declarations); testCtx is true inside an XCTestCase subclass.
func (w *swiftWalk) walk(n node, enclosing int64, inFunc, testCtx bool) {
	switch n.kind() {
	case "class_declaration": // class / struct / enum / actor / extension
		w.handleType(n, enclosing)
	case "protocol_declaration":
		w.handleProtocol(n, enclosing)
	case "function_declaration", "protocol_function_declaration":
		w.addFunc(n, enclosing, testCtx)
		w.walkChildren(n, -1, true, testCtx)
	case "init_declaration", "protocol_initializer_declaration":
		w.addNamedMember(n, enclosing, "init")
		w.walkChildren(n, -1, true, testCtx)
	case "deinit_declaration":
		w.addNamedMember(n, enclosing, "deinit")
		w.walkChildren(n, -1, true, testCtx)
	case "subscript_declaration", "protocol_subscript_declaration":
		w.addNamedMember(n, enclosing, "subscript")
		w.walkChildren(n, -1, true, testCtx)
	case "property_declaration", "protocol_property_declaration":
		if !inFunc {
			w.addProperty(n, enclosing)
		}
	case "enum_entry":
		if !inFunc {
			w.addEnumEntry(n, enclosing)
		}
	case "typealias_declaration", "protocol_typealias_declaration":
		if !inFunc {
			w.addTypealias(n, enclosing)
		}
	case "import_declaration":
		w.addImport(n)
	default:
		w.walkChildren(n, enclosing, inFunc, testCtx)
	}
}

func (w *swiftWalk) walkChildren(n node, enclosing int64, inFunc, testCtx bool) {
	for _, c := range n.children() {
		w.walk(c, enclosing, inFunc, testCtx)
	}
}

// handleType adds a struct/class/enum/actor/extension node (all KindClass) and
// walks its body. A type inheriting XCTestCase marks its body as a test context.
func (w *swiftWalk) handleType(n node, enclosing int64) {
	name := w.typeName(n)
	if name == "" {
		w.walkChildren(n, enclosing, false, false)
		return
	}
	idx := w.addType(n, name, topology.KindClass, enclosing)
	if body := n.childByFieldName("body"); !body.isNull() {
		w.walkChildren(body, idx, false, w.isTestClass(n))
	}
}

func (w *swiftWalk) handleProtocol(n node, enclosing int64) {
	name := w.typeName(n)
	if name == "" {
		w.walkChildren(n, enclosing, false, false)
		return
	}
	idx := w.addType(n, name, topology.KindType, enclosing)
	if body := n.childByFieldName("body"); !body.isNull() {
		w.walkChildren(body, idx, false, false)
	}
}

func (w *swiftWalk) addType(n node, name string, kind topology.NodeKind, enclosing int64) int64 {
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		StartLine: w.line(n.startByte()),
		EndLine:   w.line(n.endByte()),
		Language:  "swift",
		Path:      w.path,
	})
	if c := w.typeConformance(n); c != "" {
		w.conf[idx] = c
	}
	w.containedBy(enclosing, idx)
	return idx
}

func (w *swiftWalk) addFunc(n node, enclosing int64, testCtx bool) {
	name := w.funcName(n)
	if name == "" {
		return
	}
	kind := topology.KindFunction
	if enclosing >= 0 {
		kind = topology.KindMethod
	}
	if testCtx && strings.HasPrefix(name, "test") {
		kind = topology.KindTest
	}
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		Signature: w.methodSignature(n, enclosing),
		StartLine: w.line(n.startByte()),
		EndLine:   w.line(n.endByte()),
		Language:  "swift",
		Path:      w.path,
	})
	w.funcIdx[name] = idx
	w.containedBy(enclosing, idx)
}

// addNamedMember records a callable whose name is not a simple_identifier —
// init, deinit, subscript — under a fixed name. A member of a type is a method;
// at file scope (impossible for these, but handled) it is a function.
func (w *swiftWalk) addNamedMember(n node, enclosing int64, name string) {
	kind := topology.KindFunction
	if enclosing >= 0 {
		kind = topology.KindMethod
	}
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		Signature: w.methodSignature(n, enclosing),
		StartLine: w.line(n.startByte()),
		EndLine:   w.line(n.endByte()),
		Language:  "swift",
		Path:      w.path,
	})
	w.funcIdx[name] = idx
	w.containedBy(enclosing, idx)
}

// methodSignature returns the function head, suffixed with the enclosing type's
// conformance list when this is a method of a conforming type — surfacing a
// type's protocol conformance on its methods for pattern tools.
func (w *swiftWalk) methodSignature(n node, enclosing int64) string {
	sig := w.funcSignature(n)
	if enclosing >= 0 {
		if c := w.conf[enclosing]; c != "" {
			return strings.TrimSpace(sig + " " + c)
		}
	}
	return sig
}

// funcSignature returns the function head text — everything before the body
// (a function_body, or a subscript's computed_property/getter block).
func (w *swiftWalk) funcSignature(n node) string {
	var parts []string
	for _, c := range n.children() {
		if k := c.kind(); k == "function_body" || k == "computed_property" {
			break
		}
		if t := strings.TrimSpace(c.text(w.src)); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " ")
}

// addProperty records a member or top-level property. let → constant, var →
// variable.
func (w *swiftWalk) addProperty(n node, enclosing int64) {
	name := w.propertyName(n)
	if name == "" {
		return
	}
	kind := topology.KindConstant
	if w.bindingIsVar(n) {
		kind = topology.KindVariable
	}
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		StartLine: w.line(n.startByte()),
		EndLine:   w.line(n.endByte()),
		Language:  "swift",
		Path:      w.path,
	})
	w.containedBy(enclosing, idx)
}

// addEnumEntry records every case bound by one entry (`case a, b` is two).
func (w *swiftWalk) addEnumEntry(n node, enclosing int64) {
	for _, c := range n.children() {
		if c.kind() != "simple_identifier" {
			continue
		}
		idx := int64(len(w.nodes))
		w.nodes = append(w.nodes, topology.Node{
			Kind:      topology.KindConstant,
			Name:      c.text(w.src),
			Qualified: c.text(w.src),
			StartLine: w.line(n.startByte()),
			EndLine:   w.line(n.endByte()),
			Language:  "swift",
			Path:      w.path,
		})
		w.containedBy(enclosing, idx)
	}
}

// addTypealias records a `typealias Foo = Bar` declaration as a KindType.
func (w *swiftWalk) addTypealias(n node, enclosing int64) {
	nm := n.childByFieldName("name")
	if nm.isNull() || nm.kind() != "type_identifier" {
		return
	}
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      topology.KindType,
		Name:      nm.text(w.src),
		Qualified: nm.text(w.src),
		StartLine: w.line(n.startByte()),
		EndLine:   w.line(n.endByte()),
		Language:  "swift",
		Path:      w.path,
	})
	w.containedBy(enclosing, idx)
}

func (w *swiftWalk) addImport(n node) {
	id := n.childByType("identifier")
	if id.isNull() {
		return
	}
	name := strings.TrimSpace(id.text(w.src))
	if name == "" {
		return
	}
	w.nodes = append(w.nodes, topology.Node{
		Kind:      topology.KindImport,
		Name:      name,
		StartLine: w.line(n.startByte()),
		Language:  "swift",
		Path:      w.path,
	})
}

func (w *swiftWalk) containedBy(enclosing, child int64) {
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

// typeName returns a declaration's name from its `name` field: a bare
// type_identifier (class/struct/enum/actor/protocol) or the type_identifier
// inside a user_type (an extension's subject).
func (w *swiftWalk) typeName(n node) string {
	nm := n.childByFieldName("name")
	if nm.isNull() {
		return ""
	}
	switch nm.kind() {
	case "type_identifier":
		return nm.text(w.src)
	case "user_type":
		if id := nm.childByType("type_identifier"); !id.isNull() {
			return id.text(w.src)
		}
	}
	return ""
}

// funcName returns the function's name: the simple_identifier bound to its
// `name` field (function_declaration repeats `name` for the return type, but the
// first is the identifier).
func (w *swiftWalk) funcName(n node) string {
	if nm := n.childByFieldName("name"); !nm.isNull() && nm.kind() == "simple_identifier" {
		return nm.text(w.src)
	}
	if id := n.childByType("simple_identifier"); !id.isNull() {
		return id.text(w.src)
	}
	return w.operatorName(n)
}

// operatorName returns the operator token of an operator function (`static func
// + …`) — the token immediately after the `func` keyword. Empty for a normal
// function (whose name is a simple_identifier handled above).
func (w *swiftWalk) operatorName(n node) string {
	kids := n.children()
	for i, c := range kids {
		if c.kind() != "func" || i+1 >= len(kids) {
			continue
		}
		op := kids[i+1]
		t := strings.TrimSpace(op.text(w.src))
		if t != "" && t != "(" && op.kind() != "simple_identifier" {
			return t
		}
		return ""
	}
	return ""
}

// propertyName returns the bound identifier of a property: the
// `pattern`'s `bound_identifier` simple_identifier.
func (w *swiftWalk) propertyName(n node) string {
	pat := w.patternOf(n)
	if pat.isNull() {
		return ""
	}
	if id := pat.childByFieldName("bound_identifier"); !id.isNull() {
		return id.text(w.src)
	}
	if id := pat.childByType("simple_identifier"); !id.isNull() {
		return id.text(w.src)
	}
	return ""
}

// patternOf returns the property's pattern node (the `name` field, or a direct
// pattern child for shapes that don't field-tag it).
func (w *swiftWalk) patternOf(n node) node {
	if pat := n.childByFieldName("name"); !pat.isNull() {
		return pat
	}
	return n.childByType("pattern")
}

// bindingIsVar reports whether a property binds with `var`. The
// value_binding_pattern is a direct child of a property_declaration but nested
// inside the pattern of a protocol_property_declaration.
func (w *swiftWalk) bindingIsVar(n node) bool {
	if b := n.childByType("value_binding_pattern"); !b.isNull() {
		return strings.TrimSpace(b.text(w.src)) == "var"
	}
	if pat := w.patternOf(n); !pat.isNull() {
		if b := pat.childByType("value_binding_pattern"); !b.isNull() {
			return strings.TrimSpace(b.text(w.src)) == "var"
		}
	}
	return false
}

// isTestClass reports whether a type inherits XCTestCase.
func (w *swiftWalk) isTestClass(n node) bool {
	for _, c := range n.children() {
		if c.kind() == "inheritance_specifier" && strings.Contains(c.text(w.src), "XCTestCase") {
			return true
		}
	}
	return false
}

// typeConformance joins a type's inheritance/conformance specifiers' text.
func (w *swiftWalk) typeConformance(n node) string {
	var parts []string
	for _, c := range n.children() {
		if c.kind() != "inheritance_specifier" {
			continue
		}
		if t := strings.TrimSpace(c.text(w.src)); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " ")
}

// callEdges emits EdgeCalls between functions defined in the file. The call site
// is certain but the callee is resolved by name within the file (0.8 heuristic).
func (w *swiftWalk) callEdges(root node) {
	seen := map[[2]int64]bool{}
	var rec func(n node, curFunc int64)
	rec = func(n node, curFunc int64) {
		switch n.kind() {
		case "function_declaration", "protocol_function_declaration":
			if idx, ok := w.funcIdx[w.funcName(n)]; ok {
				curFunc = idx
			}
		case "call_expression":
			w.maybeCallEdge(n, curFunc, seen)
		}
		for _, c := range n.children() {
			rec(c, curFunc)
		}
	}
	rec(root, -1)
}

func (w *swiftWalk) maybeCallEdge(call node, curFunc int64, seen map[[2]int64]bool) {
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

func (w *swiftWalk) calleeName(call node) string {
	callee := call.firstNamedChild()
	if callee.isNull() {
		return ""
	}
	switch callee.kind() {
	case "simple_identifier":
		return callee.text(w.src)
	case "navigation_expression":
		if suf := callee.childByType("navigation_suffix"); !suf.isNull() {
			if id := suf.childByType("simple_identifier"); !id.isNull() {
				return id.text(w.src)
			}
		}
	}
	return ""
}
