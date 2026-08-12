package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// CSharpExtractor extracts C# symbols using the gotreesitter C# grammar.
//
// Concurrency: stateless after construction and safe for concurrent use; a
// fresh parser is created per Extract call because gotreesitter parsers are not
// safe for concurrent reuse.
type CSharpExtractor struct {
	lang lazyGrammar
}

// NewCSharp returns a tree-sitter-backed C# extractor.
func NewCSharp() *CSharpExtractor {
	return &CSharpExtractor{lang: lazyGrammar{load: grammars.CSharpLanguage}}
}

func (e *CSharpExtractor) Language() string     { return "csharp" }
func (e *CSharpExtractor) Extensions() []string { return []string{".cs"} }

// Extract parses src and returns C# namespaces (KindPackage, both the block and
// the file-scoped form, mirroring how the Go extractor treats a package),
// classes/structs/records/enums (KindClass), interfaces (KindType — a contract,
// the same concrete-vs-contract split Rust, Kotlin and Swift use), delegates
// (KindType — a named function type), methods/constructors/destructors/
// operators/indexers (KindMethod), local functions (KindFunction), properties,
// events and fields (KindConstant when the declaration is immutable — const,
// readonly, init-only or get-only — KindVariable otherwise), enum members
// (KindConstant), using directives (KindImport) and attribute-marked tests
// (KindTest), plus container → member containment edges and intra-file call
// edges. Containment is lexical and therefore certain (1.0/extractor);
// intra-file calls are name-resolved heuristics (0.8). This is the structural
// Map for C#; there is no C# LSP adapter, so it is also the only signal.
// Returns (nil, nil, nil) when src cannot be parsed.
func (e *CSharpExtractor) Extract(ctx context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	lang := e.lang.get()
	return extractWith(ctx, lang, src, func(root *tsg.Node) ([]topology.Node, []topology.Edge) {
		w := &csharpWalk{lang: lang, src: src, path: relPath, funcIdx: map[string]int64{}}
		w.walkChildren(root, -1, false)
		w.callEdges(root)
		return w.nodes, w.edges
	})
}

type csharpWalk struct {
	lang       *tsg.Language
	src        []byte
	path       string
	nodes      []topology.Node
	edges      []topology.Edge
	funcIdx    map[string]int64 // callable name → node index, for call edges
	nameCounts map[string]int   // callable Name → count, for ambiguous-call down-weight (#30)
}

// walk descends the tree. enclosing is the node index of the lexically enclosing
// namespace/type (-1 at file scope); inFunc is true once inside a method body,
// which keeps a local declaration from being recorded as a member of the type
// the body happens to sit in.
//
// The flag is threaded rather than short-circuiting the descent (the flat
// "stop at a body" style some extractors use) because the call-edge pass needs
// bodies walked in full: a `Foo()` inside a method is precisely the edge wanted,
// so the walk must reach it while still knowing it is not a member site.
func (w *csharpWalk) walk(n *tsg.Node, enclosing int64, inFunc bool) {
	switch n.Type(w.lang) {
	case "class_declaration", "struct_declaration", "record_declaration", "enum_declaration":
		w.handleType(n, topology.KindClass, enclosing)
	case "interface_declaration":
		w.handleType(n, topology.KindType, enclosing)
	case "namespace_declaration":
		w.handleType(n, topology.KindPackage, enclosing)
	case "delegate_declaration":
		w.addNamed(n, topology.KindType, enclosing)
	case "method_declaration", "constructor_declaration", "destructor_declaration",
		"operator_declaration", "conversion_operator_declaration", "indexer_declaration":
		w.addCallable(n, enclosing)
	case "local_function_statement":
		w.addLocalFunction(n)
	case "property_declaration", "event_declaration":
		w.addNamed(n, w.memberKind(n), enclosing)
	case "field_declaration", "event_field_declaration":
		if !inFunc {
			w.addVariables(n, enclosing)
		}
	case "enum_member_declaration":
		w.addEnumMember(n, enclosing)
	case "using_directive":
		w.addImport(n)
	default:
		w.walkChildren(n, enclosing, inFunc)
	}
}

// walkChildren descends into n's children, with one wrinkle: a file-scoped
// namespace (`namespace A.B;`) is not a container node at all — the grammar
// emits it as a plain sibling and every declaration that follows is a sibling
// too, not a child. So it is handled here rather than in walk: once one is seen,
// it becomes the enclosing scope for the REST of this child list, which is what
// makes containment edges come out identical for the block and file-scoped
// spellings of the same file.
func (w *csharpWalk) walkChildren(n *tsg.Node, enclosing int64, inFunc bool) {
	for _, c := range n.Children() {
		if c.Type(w.lang) == "file_scoped_namespace_declaration" {
			if name := w.declName(c); name != "" {
				enclosing = w.addNode(c, topology.KindPackage, name, enclosing, true)
			}
			continue
		}
		w.walk(c, enclosing, inFunc)
	}
}

// handleType emits a namespace or type declaration and walks its body,
// attributing members to it.
//
// When the body node is missing the children are walked anyway. That is the
// error-recovery case: a type whose body failed to close cleanly still holds
// correctly parsed member nodes, just not under a `declaration_list`, and
// walking the raw child list recovers them without any guessing — every symbol
// still comes from a real typed node.
func (w *csharpWalk) handleType(n *tsg.Node, kind topology.NodeKind, enclosing int64) {
	name := w.declName(n)
	if name == "" {
		w.walkChildren(n, enclosing, false)
		return
	}
	idx := w.addNode(n, kind, name, enclosing, true)
	if body := w.typeBody(n); body != nil {
		w.walkChildren(body, idx, false)
		return
	}
	w.walkChildren(n, idx, false)
}

// addCallable emits a method-shaped member: methods, constructors, destructors,
// operators, conversion operators and indexers.
//
// Indexers are grouped here rather than with properties because, despite the
// property-like `this[i]` call syntax, an indexer takes a parameter list and is
// an accessor pair — treating it as a member value would give it a Kind that
// says "storage" for something that is code.
func (w *csharpWalk) addCallable(n *tsg.Node, enclosing int64) {
	name := w.callableName(n)
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
	idx := w.addNode(n, kind, name, enclosing, true)
	w.funcIdx[name] = idx
	// A member declared inside a body is not a member of the enclosing type.
	w.walkChildren(n, -1, true)
}

// addLocalFunction emits a local function — one inside a method body, or at file
// scope among top-level statements. Both are KindFunction regardless of what
// encloses them, because a local function is not callable as a member; a
// file-scoped namespace would otherwise make every top-level one a method, since
// it is the enclosing scope of every top-level statement.
func (w *csharpWalk) addLocalFunction(n *tsg.Node) {
	name := w.declName(n)
	if name == "" {
		return
	}
	kind := topology.KindFunction
	if w.isTest(n) {
		kind = topology.KindTest
	}
	idx := w.addNode(n, kind, name, -1, true)
	w.funcIdx[name] = idx
	w.walkChildren(n, -1, true)
}

// addNamed emits a declaration that needs nothing beyond its own name: a
// delegate, a property or an event.
//
// Properties and events are KindConstant/KindVariable rather than KindField.
// That is topology.KindField's own contract, not a preference — KindField is a
// key of a DATA-FORMAT file (a SQL column, a TOML key), never a member of a
// code type.
func (w *csharpWalk) addNamed(n *tsg.Node, kind topology.NodeKind, enclosing int64) {
	if name := w.declName(n); name != "" {
		w.addNode(n, kind, name, enclosing, true)
	}
}

// addVariables emits one node per declarator of a field or event-field
// declaration, so `int x, y;` yields two. The span is the whole declaration for
// each, because that is the edit range a consumer needs; the declarator alone
// would not be a removable statement.
func (w *csharpWalk) addVariables(n *tsg.Node, enclosing int64) {
	kind := w.memberKind(n)
	decl := childByType(n, "variable_declaration", w.lang)
	if decl == nil {
		return
	}
	for _, c := range decl.Children() {
		if c.Type(w.lang) != "variable_declarator" {
			continue
		}
		if name := w.identifierOf(c); name != "" {
			w.addNode(n, kind, name, enclosing, false)
		}
	}
}

func (w *csharpWalk) addEnumMember(n *tsg.Node, enclosing int64) {
	if name := w.declName(n); name != "" {
		w.addNode(n, topology.KindConstant, name, enclosing, false)
	}
}

// addImport records a using directive. The name is the LAST namespace-shaped
// child, which lands on the imported namespace in all four spellings —
// `using X;`, `using static X;`, `global using X;` and the alias form
// `using A = X;`, where the alias identifier comes first and X last. The alias
// is deliberately dropped: what the file depends on is X.
func (w *csharpWalk) addImport(n *tsg.Node) {
	name := ""
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "identifier", "qualified_name", "generic_name", "alias_qualified_name":
			name = strings.TrimSpace(c.Text(w.src))
		}
	}
	if name == "" {
		return
	}
	node := topology.Node{
		Kind:      topology.KindImport,
		Name:      name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "csharp",
		Path:      w.path,
	}
	setSpan(&node, n)
	w.nodes = append(w.nodes, node)
}

// csharpIsComment reports whether a C# grammar node type is a comment. The
// grammar emits a single `comment` type for `//`, `/* */` and the `///` XML
// documentation form alike, so xmldoc needs no special case.
func csharpIsComment(typ string) bool { return typ == "comment" }

// addNode appends a node and, when it has a lexical enclosing declaration, a
// certain (1.0/extractor) containment edge. withDoc stamps a leading-comment doc
// span; it is off for fields and enum members, where several names can come out
// of one declaration node and would then share a single doc span.
func (w *csharpWalk) addNode(n *tsg.Node, kind topology.NodeKind, name string, enclosing int64, withDoc bool) int64 {
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		Signature: w.signature(n),
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "csharp",
		Path:      w.path,
	}
	setSpan(&node, n)
	if withDoc {
		node.DocStartByte, node.DocEndByte = docSpanBefore(n, w.lang, w.src, csharpIsComment)
	}
	w.nodes = append(w.nodes, node)
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

// signature reproduces the declaration head — everything from after the
// attributes up to the body — collapsed onto one line. Taking it as source text
// rather than rebuilding it from fields is what makes generics and their
// constraints come out for free: `class Repo<T> : Base<T> where T : class`
// keeps the type parameters, the base list and the constraint clause, all of
// which someone searching for the declaration is likely to type.
func (w *csharpWalk) signature(n *tsg.Node) string {
	start, end := n.StartByte(), n.EndByte()
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "attribute_list":
			if c.EndByte() > start {
				start = c.EndByte()
			}
		case "declaration_list", "block", "accessor_list", "arrow_expression_clause",
			"enum_member_declaration_list", "constructor_initializer", ";":
			if c.StartByte() < end {
				end = c.StartByte()
			}
		}
	}
	lo, hi := clampU32(start), clampU32(end)
	if hi > len(w.src) {
		hi = len(w.src)
	}
	if lo > hi {
		return ""
	}
	return normaliseSpace(string(w.src[lo:hi]))
}

// memberKind decides whether a property, event or field is immutable. C# spells
// immutability four ways and they are not interchangeable, so all four count: a
// `const`/`readonly` modifier; no `set` accessor (get-only); `init`, which reads
// like a setter but only runs during construction; and the expression body
// `public string Name => _name;`, which is get-only because there is nowhere to
// put a setter.
//
// Events are the deliberate exception. Their accessor list holds `add`/`remove`
// rather than `get`/`set`, so the no-setter rule would call every event
// immutable — when subscribing to one is exactly the mutation it exists for.
// Only an explicit modifier makes an event a constant.
func (w *csharpWalk) memberKind(n *tsg.Node) topology.NodeKind {
	for _, c := range n.Children() {
		if c.Type(w.lang) != "modifier" {
			continue
		}
		switch strings.TrimSpace(c.Text(w.src)) {
		case "const", "readonly":
			return topology.KindConstant
		}
	}
	switch n.Type(w.lang) {
	case "event_declaration", "event_field_declaration", "field_declaration":
		return topology.KindVariable
	}
	if childByType(n, "arrow_expression_clause", w.lang) != nil {
		return topology.KindConstant
	}
	accessors := childByType(n, "accessor_list", w.lang)
	if accessors == nil {
		return topology.KindVariable
	}
	for _, a := range accessors.Children() {
		if a.Type(w.lang) != "accessor_declaration" {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(a.Text(w.src)), "set") {
			return topology.KindVariable
		}
	}
	return topology.KindConstant
}

// callableName names a method-shaped member. Most carry a `name` field; the
// three that do not are named after their source spelling so they stay
// distinguishable in an index: an operator as `operator +`, a conversion
// operator by its target type, and an indexer as `this[]`.
//
// A destructor is deliberately prefixed with `~`. The grammar gives it the same
// `name` field as the constructor — both are the type's own name — so without
// the prefix the two would collide in funcIdx and one would silently replace
// the other, taking its call edges with it.
func (w *csharpWalk) callableName(n *tsg.Node) string {
	switch n.Type(w.lang) {
	case "destructor_declaration":
		if name := w.declName(n); name != "" {
			return "~" + name
		}
		return ""
	case "operator_declaration":
		if op := n.ChildByFieldName("operator", w.lang); op != nil {
			return "operator " + strings.TrimSpace(op.Text(w.src))
		}
		return "operator"
	case "conversion_operator_declaration":
		if t := n.ChildByFieldName("type", w.lang); t != nil {
			return "operator " + normaliseSpace(t.Text(w.src))
		}
		return "operator"
	case "indexer_declaration":
		return "this[]"
	}
	return w.declName(n)
}

// declName returns a declaration's name, from the grammar's `name` field where
// that resolves and from the child list where it does not.
//
// The fallback is load-bearing, not belt-and-braces. In this grammar a single
// parse error anywhere in a FILE knocks out the `name` field of every
// class_declaration in it — including ones that parsed perfectly and sit before
// the damage, so `public class Good { }` above a broken class has a nil name
// field. Without the fallback one unsupported construct costs a real file all of
// its types, and since a nameless type emits nothing while passing its parent's
// scope down, every method in it silently becomes an unattached KindFunction
// instead of a method. Member name fields keep working, which is what makes the
// failure quiet rather than empty.
func (w *csharpWalk) declName(n *tsg.Node) string {
	if nm := n.ChildByFieldName("name", w.lang); nm != nil {
		return strings.TrimSpace(nm.Text(w.src))
	}
	return w.headName(n)
}

// headName recovers a declaration's name positionally: the last identifier or
// qualified name in the declaration HEAD, the head being everything before
// whatever opens the body, the parameters or the type arguments.
//
// "Last in the head" is what makes one rule serve every declaration shape.
// A type's head holds only its name (`class Repo` → Repo); a member's head
// holds its return type first and its name last (`async Task DoesThing` →
// DoesThing, `Foo Bar { get; }` → Bar), and a predefined type such as `void` is
// not an identifier node at all, so it never competes.
func (w *csharpWalk) headName(n *tsg.Node) string {
	name := ""
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "identifier", "qualified_name":
			name = strings.TrimSpace(c.Text(w.src))
		case "parameter_list", "bracketed_parameter_list", "type_parameter_list",
			"base_list", "declaration_list", "accessor_list", "block",
			"arrow_expression_clause", "enum_member_declaration_list",
			"variable_declaration", ";":
			return name
		}
	}
	return name
}

func (w *csharpWalk) identifierOf(n *tsg.Node) string {
	if id := childByType(n, "identifier", w.lang); id != nil {
		return id.Text(w.src)
	}
	return ""
}

func (w *csharpWalk) typeBody(n *tsg.Node) *tsg.Node {
	if b := childByType(n, "declaration_list", w.lang); b != nil {
		return b
	}
	return childByType(n, "enum_member_declaration_list", w.lang)
}

// isTest reports whether a declaration carries a test-framework attribute. The
// set spans the three frameworks a C# repository is likely to use — xunit
// (`Fact`, `Theory`), NUnit (`Test`, `TestCase`) and MSTest (`TestMethod`) —
// plus any name ending in `Test`, which catches the derived attributes each
// framework encourages (`SkippableTest`, `PropertyTest`, …). An attribute may
// be written with or without its `Attribute` suffix and may be qualified, so
// the deepest-rightmost identifier is taken and the suffix trimmed.
func (w *csharpWalk) isTest(n *tsg.Node) bool {
	for _, c := range n.Children() {
		if c.Type(w.lang) != "attribute_list" {
			continue
		}
		for _, a := range c.Children() {
			if a.Type(w.lang) != "attribute" {
				continue
			}
			if isCSharpTestAttribute(lastCSharpIdentifier(a, w.lang, w.src)) {
				return true
			}
		}
	}
	return false
}

func isCSharpTestAttribute(name string) bool {
	name = strings.TrimSuffix(name, "Attribute")
	switch name {
	case "Fact", "Theory", "Test", "TestCase", "TestMethod":
		return true
	}
	return strings.HasSuffix(name, "Test")
}

// callEdges does a second pass emitting EdgeCalls between callables defined in
// the file. The call site is syntactically certain but the callee is resolved by
// name within the file, so confidence is 0.8 (heuristic).
func (w *csharpWalk) callEdges(root *tsg.Node) {
	seen := map[[2]int64]bool{}
	w.nameCounts = callableNameCounts(w.nodes)
	walkCallSites(root,
		scopeByType(w.lang, w.funcIdx, w.callableName,
			"method_declaration", "constructor_declaration", "destructor_declaration",
			"operator_declaration", "conversion_operator_declaration", "indexer_declaration",
			"local_function_statement"),
		func(n *tsg.Node, curFunc int64) {
			if n.Type(w.lang) == "invocation_expression" {
				w.maybeCallEdge(n, curFunc, seen)
			}
		})
}

func (w *csharpWalk) maybeCallEdge(call *tsg.Node, curFunc int64, seen map[[2]int64]bool) {
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
	w.edges = append(w.edges, heuristicCallEdge(curFunc, to, w.nodes, w.nameCounts))
}

// calleeName reads the called name out of an invocation's `function` field:
// a bare `Foo()`, a receiver-qualified `x.Foo()` (the member name only — the
// extractor has no type information, which is why the resulting edge is a
// heuristic), or a generic `Foo<T>()`.
func (w *csharpWalk) calleeName(call *tsg.Node) string {
	fn := call.ChildByFieldName("function", w.lang)
	if fn == nil {
		return ""
	}
	switch fn.Type(w.lang) {
	case "identifier":
		return fn.Text(w.src)
	case "member_access_expression":
		if nm := fn.ChildByFieldName("name", w.lang); nm != nil {
			return w.simpleName(nm)
		}
	case "generic_name":
		return w.identifierOf(fn)
	}
	return ""
}

// simpleName reduces a call target to its bare identifier, unwrapping the
// generic form so `x.Map<T>()` resolves to `Map`.
func (w *csharpWalk) simpleName(n *tsg.Node) string {
	if n.Type(w.lang) == "generic_name" {
		return w.identifierOf(n)
	}
	return n.Text(w.src)
}

// lastCSharpIdentifier returns the deepest-rightmost identifier under n, so a
// qualified attribute like `[Xunit.Fact]` yields its simple name "Fact".
func lastCSharpIdentifier(n *tsg.Node, lang *tsg.Language, src []byte) string {
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
