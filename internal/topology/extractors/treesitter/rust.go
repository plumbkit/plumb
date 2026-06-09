package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// RustExtractor extracts Rust symbols using the gotreesitter Rust grammar.
//
// Concurrency: stateless after construction and safe for concurrent use; a
// fresh parser is created per Extract call because gotreesitter parsers are not
// safe for concurrent reuse.
type RustExtractor struct {
	lang lazyGrammar
}

// NewRust returns a tree-sitter-backed Rust extractor.
func NewRust() *RustExtractor {
	return &RustExtractor{lang: lazyGrammar{load: grammars.RustLanguage}}
}

func (e *RustExtractor) Language() string     { return "rust" }
func (e *RustExtractor) Extensions() []string { return []string{".rs"} }

// Extract parses src and returns Rust functions, methods, types (struct, enum,
// union, trait, type-alias), constants, imports and tests, plus containment and
// intra-file call edges. Lexical containment (trait → method signature) is
// certain (1.0); impl-method → type links are name-resolved within the file and
// so are heuristic (0.8), as are intra-file call edges. Returns (nil, nil, nil)
// when src cannot be parsed.
func (e *RustExtractor) Extract(_ context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	tree, err := tsg.NewParser(e.lang.get()).Parse(src)
	if err != nil || tree == nil {
		return nil, nil, nil
	}
	defer tree.Release()
	w := &rustWalk{
		lang:    e.lang.get(),
		src:     src,
		path:    relPath,
		funcIdx: map[string]int64{},
		typeIdx: map[string]int64{},
	}
	w.walk(tree.RootNode(), -1, false)
	w.resolveImplContains()
	w.callEdges(tree.RootNode())
	return w.nodes, w.edges, nil
}

type rustWalk struct {
	lang    *tsg.Language
	src     []byte
	path    string
	nodes   []topology.Node
	edges   []topology.Edge
	funcIdx map[string]int64   // function/method/test name → node index, for call edges
	typeIdx map[string]int64   // type name → node index, for impl-method linking
	pending []rustImplContains // impl-method → type name, resolved after the walk
}

type rustImplContains struct {
	typeName string
	memberID int64
}

func (w *rustWalk) fieldText(n *tsg.Node, field string) string {
	if c := n.ChildByFieldName(field, w.lang); c != nil {
		return c.Text(w.src)
	}
	return ""
}

// walk descends the tree. Inside a function body (inFunc) only nested `fn`s and
// `use` imports surface (matching the other extractors); every other
// declaration there is a local and is skipped. Top-level declarations are
// handled by walkItem.
func (w *rustWalk) walk(n *tsg.Node, enclosingType int64, inFunc bool) {
	switch n.Type(w.lang) {
	case "function_item", "function_signature_item":
		w.addFunc(n, enclosingType)
		w.walkChildren(n, -1, true)
		return
	case "use_declaration":
		w.addImport(n)
		return
	}
	if inFunc {
		w.walkChildren(n, enclosingType, true)
		return
	}
	w.walkItem(n, enclosingType)
}

// walkItem records a top-level (non-function) declaration. Only reached outside
// a function body, so no inFunc guard is needed here.
func (w *rustWalk) walkItem(n *tsg.Node, enclosingType int64) {
	switch n.Type(w.lang) {
	case "struct_item", "union_item":
		idx := w.addType(n)
		w.addStructFields(n, idx)
	case "enum_item":
		idx := w.addType(n)
		w.addEnumVariants(n, idx)
	case "type_item":
		w.addType(n)
	case "trait_item":
		idx := w.addType(n)
		if body := n.ChildByFieldName("body", w.lang); body != nil {
			w.walkChildren(body, idx, false)
		}
	case "const_item":
		w.addBinding(n, topology.KindConstant)
	case "static_item":
		w.addBinding(n, topology.KindVariable)
	case "impl_item":
		w.walkImpl(n)
	case "mod_item":
		if body := n.ChildByFieldName("body", w.lang); body != nil {
			w.walkChildren(body, -1, false)
		}
	default:
		w.walkChildren(n, enclosingType, false)
	}
}

func (w *rustWalk) walkChildren(n *tsg.Node, enclosingType int64, inFunc bool) {
	for _, c := range n.Children() {
		w.walk(c, enclosingType, inFunc)
	}
}

func (w *rustWalk) addType(n *tsg.Node) int64 {
	name := w.fieldText(n, "name")
	if name == "" {
		return -1
	}
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      topology.KindType,
		Name:      name,
		Qualified: name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "rust",
		Path:      w.path,
	})
	w.typeIdx[name] = idx
	return idx
}

func (w *rustWalk) addFunc(n *tsg.Node, enclosingType int64) {
	name := w.fieldText(n, "name")
	if name == "" {
		return
	}
	kind := topology.KindFunction
	if enclosingType >= 0 {
		kind = topology.KindMethod
	}
	if w.hasTestAttr(n) {
		kind = topology.KindTest
	}
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "rust",
		Path:      w.path,
	})
	w.funcIdx[name] = idx
	if enclosingType >= 0 {
		// Lexically contained (trait default/signature method): certain.
		w.edges = append(w.edges, topology.Edge{
			FromID:     enclosingType,
			ToID:       idx,
			Kind:       topology.EdgeContains,
			Confidence: 1.0,
			Source:     "extractor",
		})
	}
}

// walkImpl records the methods of an `impl` block. The method is lexically
// inside the impl, but linking it to the implemented type's node is a name
// match within the file, so the containment edge is heuristic (0.8) and
// deferred until every type node has been seen (resolveImplContains).
func (w *rustWalk) walkImpl(n *tsg.Node) {
	typeName := ""
	if t := n.ChildByFieldName("type", w.lang); t != nil {
		typeName = baseTypeName(t, w.lang, w.src)
	}
	body := n.ChildByFieldName("body", w.lang)
	if body == nil {
		return
	}
	for _, c := range body.Children() {
		var id int64 = -1
		switch c.Type(w.lang) {
		case "function_item":
			id = w.addMethod(c)
			w.walkChildren(c, -1, true)
		case "const_item":
			id = w.addBinding(c, topology.KindConstant)
		case "type_item":
			if name := w.fieldText(c, "name"); name != "" {
				id = w.addContained(c, topology.KindType, name, -1)
			}
		default:
			continue
		}
		if id >= 0 && typeName != "" {
			w.pending = append(w.pending, rustImplContains{typeName: typeName, memberID: id})
		}
	}
}

// addStructFields records each named field of a struct/union as a KindVariable
// contained in the type (Rust fields carry no per-field mutability marker).
// Tuple-struct fields (ordered_field_declaration_list) and enum-variant fields
// are deliberately skipped — they have no field name.
func (w *rustWalk) addStructFields(n *tsg.Node, typeIdx int64) {
	if typeIdx < 0 {
		return
	}
	body := n.ChildByFieldName("body", w.lang)
	if body == nil || body.Type(w.lang) != "field_declaration_list" {
		return
	}
	for _, c := range body.Children() {
		if c.Type(w.lang) != "field_declaration" {
			continue
		}
		if name := w.fieldText(c, "name"); name != "" {
			w.addContained(c, topology.KindVariable, name, typeIdx)
		}
	}
}

// addEnumVariants records each enum variant as a KindConstant contained in the
// enum type, matching the enum-member handling of the other extractors. A
// struct-variant's inner fields (the `{ x: i32 }`) are not surfaced.
func (w *rustWalk) addEnumVariants(n *tsg.Node, typeIdx int64) {
	if typeIdx < 0 {
		return
	}
	body := n.ChildByFieldName("body", w.lang)
	if body == nil || body.Type(w.lang) != "enum_variant_list" {
		return
	}
	for _, c := range body.Children() {
		if c.Type(w.lang) != "enum_variant" {
			continue
		}
		if name := w.fieldText(c, "name"); name != "" {
			w.addContained(c, topology.KindConstant, name, typeIdx)
		}
	}
}

// addContained appends a member node and a certain (1.0/extractor) containment
// edge to its enclosing type. With parent < 0 it adds the node only (the impl
// path defers the heuristic 0.8 edge to resolveImplContains).
func (w *rustWalk) addContained(n *tsg.Node, kind topology.NodeKind, name string, parent int64) int64 {
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "rust",
		Path:      w.path,
	})
	if parent >= 0 {
		w.edges = append(w.edges, topology.Edge{
			FromID:     parent,
			ToID:       idx,
			Kind:       topology.EdgeContains,
			Confidence: 1.0,
			Source:     "extractor",
		})
	}
	return idx
}

func (w *rustWalk) addMethod(n *tsg.Node) int64 {
	name := w.fieldText(n, "name")
	if name == "" {
		return -1
	}
	kind := topology.KindMethod
	if w.hasTestAttr(n) {
		kind = topology.KindTest
	}
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "rust",
		Path:      w.path,
	})
	w.funcIdx[name] = idx
	return idx
}

func (w *rustWalk) addBinding(n *tsg.Node, kind topology.NodeKind) int64 {
	name := w.fieldText(n, "name")
	if name == "" {
		return -1
	}
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "rust",
		Path:      w.path,
	})
	return idx
}

func (w *rustWalk) addImport(n *tsg.Node) {
	name := strings.TrimSpace(w.fieldText(n, "argument"))
	if name == "" {
		return
	}
	w.nodes = append(w.nodes, topology.Node{
		Kind:      topology.KindImport,
		Name:      name,
		StartLine: line(n.StartPoint()),
		Language:  "rust",
		Path:      w.path,
	})
}

func (w *rustWalk) resolveImplContains() {
	for _, p := range w.pending {
		tid, ok := w.typeIdx[p.typeName]
		if !ok {
			continue
		}
		w.edges = append(w.edges, topology.Edge{
			FromID:     tid,
			ToID:       p.memberID,
			Kind:       topology.EdgeContains,
			Confidence: 0.8,
			Source:     "heuristic",
		})
	}
}

// hasTestAttr reports whether a #[test]-family attribute precedes the item.
// Attributes are preceding siblings of the function; comments between an
// attribute and the item are skipped.
func (w *rustWalk) hasTestAttr(n *tsg.Node) bool {
	for s := n.PrevSibling(); s != nil; s = s.PrevSibling() {
		switch s.Type(w.lang) {
		case "attribute_item":
			if w.attrIsTest(s) {
				return true
			}
		case "line_comment", "block_comment":
			continue
		default:
			return false
		}
	}
	return false
}

// attrIsTest matches `#[test]` and namespaced forms like `#[tokio::test]` by
// inspecting the attribute path, so `#[cfg(test)]` (where "test" is an
// argument, not the path) does not falsely register as a test.
func (w *rustWalk) attrIsTest(item *tsg.Node) bool {
	attr := childByType(item, "attribute", w.lang)
	if attr == nil {
		return false
	}
	path := firstNamedChild(attr)
	if path == nil {
		return false
	}
	switch path.Type(w.lang) {
	case "identifier":
		return path.Text(w.src) == "test"
	case "scoped_identifier":
		return lastSegment(path.Text(w.src)) == "test"
	}
	return false
}

// callEdges does a second pass emitting EdgeCalls between functions defined in
// the file. The call site is syntactically certain but the callee is resolved
// by name within the file, so confidence is 0.8 (heuristic).
func (w *rustWalk) callEdges(root *tsg.Node) {
	seen := map[[2]int64]bool{}
	var rec func(n *tsg.Node, curFunc int64)
	rec = func(n *tsg.Node, curFunc int64) {
		switch n.Type(w.lang) {
		case "function_item", "function_signature_item":
			if idx, ok := w.funcIdx[w.fieldText(n, "name")]; ok {
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

func (w *rustWalk) maybeCallEdge(call *tsg.Node, curFunc int64, seen map[[2]int64]bool) {
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

func (w *rustWalk) calleeName(fn *tsg.Node) string {
	switch fn.Type(w.lang) {
	case "identifier":
		return fn.Text(w.src)
	case "scoped_identifier":
		return lastSegment(fn.Text(w.src))
	case "field_expression":
		if f := fn.ChildByFieldName("field", w.lang); f != nil {
			return f.Text(w.src)
		}
	}
	return ""
}

// baseTypeName returns the underlying type identifier of a type node, peeling
// references and generics (e.g. `&Foo<T>` → "Foo").
func baseTypeName(t *tsg.Node, lang *tsg.Language, src []byte) string {
	if t.Type(lang) == "type_identifier" {
		return t.Text(src)
	}
	for _, c := range t.Children() {
		if name := baseTypeName(c, lang, src); name != "" {
			return name
		}
	}
	return ""
}
