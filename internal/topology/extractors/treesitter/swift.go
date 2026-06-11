package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// SwiftExtractor extracts Swift symbols using the gotreesitter Swift grammar.
//
// Concurrency: stateless after construction and safe for concurrent use; a
// fresh parser is created per Extract call because gotreesitter parsers are not
// safe for concurrent reuse.
type SwiftExtractor struct {
	lang lazyGrammar
}

// NewSwift returns a tree-sitter-backed Swift extractor.
func NewSwift() *SwiftExtractor {
	return &SwiftExtractor{lang: lazyGrammar{load: grammars.SwiftLanguage}}
}

func (e *SwiftExtractor) Language() string     { return "swift" }
func (e *SwiftExtractor) Extensions() []string { return []string{".swift"} }

// Extract parses src and returns Swift types (struct/class/enum/actor and
// extensions, all KindClass), protocols (KindType — a contract, mirroring the
// Rust trait / Kotlin interface mapping), functions, methods, member and
// top-level properties (let → constant, var → variable), enum cases (constants),
// imports, and XCTest tests (methods named test… inside an XCTestCase subclass),
// plus container → member containment edges and intra-file call edges.
// Containment is lexical and certain (1.0/extractor); intra-file calls are
// name-resolved heuristics (0.8). A method's signature is suffixed with the
// enclosing type's conformance list, so pattern tools can see a type's protocol
// conformance (e.g. ParsableCommand) on its methods. Returns (nil, nil, nil)
// when src cannot be parsed.
func (e *SwiftExtractor) Extract(_ context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	lang := e.lang.get()
	tree, err := tsg.NewParser(lang).Parse(src)
	if err != nil || tree == nil {
		return nil, nil, nil
	}
	// The pinned gotreesitter Swift grammar cannot parse an implicitly-unwrapped
	// optional type (`var x: T!`): it emits an ERROR that cascades up and
	// collapses the enclosing class/struct declaration, so the whole type and all
	// its members are dropped from the outline. `T!` is pervasive in AppKit/UIKit
	// (`@IBOutlet var label: NSTextField!`, `var manager: Manager!`). When the
	// parse errors, blank just the offending `!` bytes — preserving every other
	// byte, so line/column offsets stay exact — and reparse the recovered source.
	if tree.RootNode().HasError() {
		if patched := recoverIUOBangs(lang, src); patched != nil {
			if t2, e2 := tsg.NewParser(lang).Parse(patched); e2 == nil && t2 != nil {
				tree.Release()
				tree, src = t2, patched
			}
		}
	}
	defer tree.Release()
	w := &swiftWalk{lang: lang, src: src, path: relPath, funcIdx: map[string]int64{}, conf: map[int64]string{}}
	w.walk(tree.RootNode(), -1, false, false)
	w.callEdges(tree.RootNode())
	return w.nodes, w.edges, nil
}

// recoverIUOBangs works around the grammar's inability to parse implicitly-
// unwrapped optional types. While the parse still has ERROR nodes that are a
// lone `!` (the grammar's failure marker for `T!`), it blanks those `!` bytes
// and reparses, bounded to a few passes so a pathological file cannot loop.
// Blanking is byte-for-byte (one `!` → one space), so node offsets and line
// numbers are identical to the original source. Returns nil when there is no
// such error to recover (a non-IUO parse error, or a clean file), leaving the
// original tree untouched.
func recoverIUOBangs(lang *tsg.Language, src []byte) []byte {
	const maxPasses = 5
	cur := src
	patched := false
	for pass := 0; pass < maxPasses; pass++ {
		tree, err := tsg.NewParser(lang).Parse(cur)
		if err != nil || tree == nil {
			break
		}
		var ranges [][2]uint32
		if tree.RootNode().HasError() {
			collectErrorBangs(tree.RootNode(), cur, &ranges)
		}
		tree.Release()
		if len(ranges) == 0 {
			break
		}
		if !patched {
			cur = append([]byte(nil), cur...)
			patched = true
		}
		for _, r := range ranges {
			for i := r[0]; i < r[1] && int(i) < len(cur); i++ {
				if cur[i] == '!' {
					cur[i] = ' '
				}
			}
		}
	}
	if !patched {
		return nil
	}
	return cur
}

// collectErrorBangs records the byte ranges of ERROR nodes whose text is a lone
// `!` — the grammar's marker for an implicitly-unwrapped optional type it could
// not parse. Force-unwrap and `!=` in valid code parse cleanly, so they are not
// ERROR nodes and are never touched.
func collectErrorBangs(n *tsg.Node, src []byte, out *[][2]uint32) {
	if n.IsError() && strings.TrimSpace(n.Text(src)) == "!" {
		*out = append(*out, [2]uint32{n.StartByte(), n.EndByte()})
		return
	}
	for _, c := range n.Children() {
		collectErrorBangs(c, src, out)
	}
}

type swiftWalk struct {
	lang    *tsg.Language
	src     []byte
	path    string
	nodes   []topology.Node
	edges   []topology.Edge
	funcIdx map[string]int64 // function/method/test name → node index, for call edges
	conf    map[int64]string // type node index → its conformance list text, for method signatures
}

// walk descends the tree. enclosing is the node index of the lexically enclosing
// type (-1 at file scope); inFunc is true once inside a function body (suppresses
// local declarations); testCtx is true inside an XCTestCase subclass (so test…
// methods become KindTest).
func (w *swiftWalk) walk(n *tsg.Node, enclosing int64, inFunc, testCtx bool) {
	switch n.Type(w.lang) {
	case "class_declaration":
		w.handleType(n, enclosing)
	case "protocol_declaration":
		w.handleProtocol(n, enclosing)
	case "function_declaration", "protocol_function_declaration":
		w.addFunc(n, enclosing, testCtx)
		w.walkChildren(n, -1, true, testCtx)
	case "property_declaration", "protocol_property_declaration":
		if !inFunc {
			w.addProperty(n, enclosing)
		}
	case "enum_entry":
		if !inFunc {
			w.addEnumEntry(n, enclosing)
		}
	case "import_declaration":
		w.addImport(n)
	default:
		w.walkChildren(n, enclosing, inFunc, testCtx)
	}
}

func (w *swiftWalk) walkChildren(n *tsg.Node, enclosing int64, inFunc, testCtx bool) {
	for _, c := range n.Children() {
		w.walk(c, enclosing, inFunc, testCtx)
	}
}

// handleType adds a struct/class/enum/actor/extension node (all KindClass) and
// walks its body. A class inheriting XCTestCase marks its body as a test context.
func (w *swiftWalk) handleType(n *tsg.Node, enclosing int64) {
	name := w.typeName(n)
	if name == "" {
		w.walkChildren(n, enclosing, false, false)
		return
	}
	idx := w.addType(n, name, topology.KindClass, enclosing)
	if body := w.typeBody(n); body != nil {
		w.walkChildren(body, idx, false, w.isTestClass(n))
	}
}

func (w *swiftWalk) handleProtocol(n *tsg.Node, enclosing int64) {
	name := w.typeName(n)
	if name == "" {
		w.walkChildren(n, enclosing, false, false)
		return
	}
	idx := w.addType(n, name, topology.KindType, enclosing)
	if body := childByType(n, "protocol_body", w.lang); body != nil {
		w.walkChildren(body, idx, false, false)
	}
}

func (w *swiftWalk) addType(n *tsg.Node, name string, kind topology.NodeKind, enclosing int64) int64 {
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "swift",
		Path:      w.path,
	})
	if c := w.typeConformance(n); c != "" {
		w.conf[idx] = c
	}
	w.containedBy(enclosing, idx)
	return idx
}

func (w *swiftWalk) addFunc(n *tsg.Node, enclosing int64, testCtx bool) {
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
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "swift",
		Path:      w.path,
	})
	w.funcIdx[name] = idx
	w.containedBy(enclosing, idx)
}

// methodSignature returns the function head, suffixed with the enclosing type's
// conformance list when this is a method of a conforming type. This surfaces a
// type's protocol conformance (e.g. ParsableCommand) on its methods so pattern
// tools like topology_routes can match an entry point by the type it conforms to.
func (w *swiftWalk) methodSignature(n *tsg.Node, enclosing int64) string {
	sig := w.funcSignature(n)
	if enclosing >= 0 {
		if c := w.conf[enclosing]; c != "" {
			return strings.TrimSpace(sig + " " + c)
		}
	}
	return sig
}

// funcSignature returns the function head text — everything before the opening
// brace of the body. For protocol declarations (no body), the full node text is
// returned. The result is used for pattern-matching in topology_routes and
// similar tools (e.g. detecting "RoutesBuilder" in a Vapor RouteCollection).
func (w *swiftWalk) funcSignature(n *tsg.Node) string {
	var parts []string
	for _, c := range n.Children() {
		if c.Type(w.lang) == "function_body" {
			break
		}
		if t := strings.TrimSpace(c.Text(w.src)); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " ")
}

// addProperty records a member or top-level property. let → constant, var →
// variable.
func (w *swiftWalk) addProperty(n *tsg.Node, enclosing int64) {
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
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "swift",
		Path:      w.path,
	})
	w.containedBy(enclosing, idx)
}

func (w *swiftWalk) addEnumEntry(n *tsg.Node, enclosing int64) {
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
		Language:  "swift",
		Path:      w.path,
	})
	w.containedBy(enclosing, idx)
}

func (w *swiftWalk) addImport(n *tsg.Node) {
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

// typeName returns a declaration's name: the direct type_identifier for
// struct/class/enum/actor/protocol, or the user_type's identifier for an
// extension (whose subject is wrapped in user_type, not a bare type_identifier).
func (w *swiftWalk) typeName(n *tsg.Node) string {
	if id := childByType(n, "type_identifier", w.lang); id != nil {
		return id.Text(w.src)
	}
	if ut := childByType(n, "user_type", w.lang); ut != nil {
		if id := childByType(ut, "type_identifier", w.lang); id != nil {
			return id.Text(w.src)
		}
	}
	return ""
}

func (w *swiftWalk) typeBody(n *tsg.Node) *tsg.Node {
	if b := childByType(n, "class_body", w.lang); b != nil {
		return b
	}
	return childByType(n, "enum_class_body", w.lang)
}

// funcName returns the function's name (its direct simple_identifier child).
func (w *swiftWalk) funcName(n *tsg.Node) string {
	if id := childByType(n, "simple_identifier", w.lang); id != nil {
		return id.Text(w.src)
	}
	return ""
}

// propertyName returns the bound identifier of a property: the simple_identifier
// inside the declaration's pattern.
func (w *swiftWalk) propertyName(n *tsg.Node) string {
	pat := childByType(n, "pattern", w.lang)
	if pat == nil {
		return ""
	}
	if id := childByType(pat, "simple_identifier", w.lang); id != nil {
		return id.Text(w.src)
	}
	return ""
}

// bindingIsVar reports whether a property binds with `var` (mutable). The
// value_binding_pattern carrying the let/var keyword is a direct child of a
// property_declaration but is nested inside the pattern for a protocol property.
func (w *swiftWalk) bindingIsVar(n *tsg.Node) bool {
	if b := childByType(n, "value_binding_pattern", w.lang); b != nil {
		return strings.TrimSpace(b.Text(w.src)) == "var"
	}
	if pat := childByType(n, "pattern", w.lang); pat != nil {
		if b := childByType(pat, "value_binding_pattern", w.lang); b != nil {
			return strings.TrimSpace(b.Text(w.src)) == "var"
		}
	}
	return false
}

// isTestClass reports whether a type declaration inherits XCTestCase (so its
// test… methods are tests).
func (w *swiftWalk) isTestClass(n *tsg.Node) bool {
	for _, c := range n.Children() {
		if c.Type(w.lang) != "inheritance_specifier" {
			continue
		}
		if strings.Contains(c.Text(w.src), "XCTestCase") {
			return true
		}
	}
	return false
}

// typeConformance returns the text of a type declaration's inheritance/conformance
// specifiers (superclass + protocols), joined by spaces. Empty when the type
// declares no conformances.
func (w *swiftWalk) typeConformance(n *tsg.Node) string {
	var parts []string
	for _, c := range n.Children() {
		if c.Type(w.lang) != "inheritance_specifier" {
			continue
		}
		if t := strings.TrimSpace(c.Text(w.src)); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " ")
}

// callEdges does a second pass emitting EdgeCalls between functions defined in
// the file. The call site is syntactically certain but the callee is resolved by
// name within the file, so confidence is 0.8 (heuristic).
func (w *swiftWalk) callEdges(root *tsg.Node) {
	seen := map[[2]int64]bool{}
	var rec func(n *tsg.Node, curFunc int64)
	rec = func(n *tsg.Node, curFunc int64) {
		switch n.Type(w.lang) {
		case "function_declaration", "protocol_function_declaration":
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

func (w *swiftWalk) maybeCallEdge(call *tsg.Node, curFunc int64, seen map[[2]int64]bool) {
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

func (w *swiftWalk) calleeName(call *tsg.Node) string {
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
