package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// CppExtractor extracts C++ symbols using the gotreesitter C++ grammar.
//
// Concurrency: stateless after construction and safe for concurrent use; a fresh
// parser is created per Extract call because gotreesitter parsers are not safe
// for concurrent reuse.
type CppExtractor struct {
	lang lazyGrammar
}

// NewCpp returns a tree-sitter-backed C++ extractor.
func NewCpp() *CppExtractor {
	return &CppExtractor{lang: lazyGrammar{load: grammars.CppLanguage}}
}

func (e *CppExtractor) Language() string { return "cpp" }

func (e *CppExtractor) Extensions() []string {
	return []string{".cc", ".cpp", ".cxx", ".hh", ".hpp", ".hxx"}
}

// Extract parses src and returns what someone navigates a C++ codebase by:
// classes and structs with their methods, fields and nested types; free
// functions and their out-of-line member definitions; enums with their
// enumerators; `using`/`typedef` aliases and concepts; file-scope variables and
// constants; `#define` and `#include`; and gtest/Boost test bodies — with
// certain (1.0) containment edges from a type to its members and heuristic call
// edges between callables in the same file. Returns (nil, nil, nil) when src
// cannot be parsed.
//
// Two things are deliberately NOT emitted. A namespace produces no symbol of its
// own: it is re-opened in every file that contributes to it, so indexing it
// would put a node named `detail` (or `std`) in a large fraction of a codebase's
// files while adding nothing a reader navigates to. Its name is not lost — it
// becomes the `::`-joined Qualified prefix of everything declared inside, which
// is what search matches on. And `using namespace X;` is not an import: it names
// no file, so an import node for it would not resolve to anything the graph
// holds.
//
// An in-class method declaration and its out-of-line definition in the same file
// are two nodes, unlike C's prototype/definition pair which collapses to one.
// They are two places a reader goes — the declaration carries the class
// containment and the doc comment, the definition carries the body — and
// collapsing them would have to drop one of those. The C hold-back still applies
// to a file-scope prototype, where the declaration genuinely carries nothing the
// definition does not.
func (e *CppExtractor) Extract(ctx context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	lang := e.lang.get()
	return extractWith(ctx, lang, src, func(root *tsg.Node) ([]topology.Node, []topology.Edge) {
		w := &cppWalk{cWalk: cWalk{
			lang: lang, src: src, path: relPath, langName: "cpp",
			funcIdx: map[string]int64{}, defined: map[string]bool{},
		}}
		for _, c := range root.Children() {
			w.top(c, cppScope{parent: -1})
		}
		w.flushPrototypes()
		w.callEdges(root)
		return w.nodes, w.edges
	})
}

// cppWalk embeds cWalk to share the emitters C++ inherits unchanged from C —
// `#include`, `#define`, `typedef`, the signature slice, the prototype hold-back,
// containment links — and re-owns everything that either recurses or resolves a
// name.
//
// Re-owning the recursion is the load-bearing part. Go has no virtual dispatch,
// so cWalk.top recurses into *cWalk's* switch: handing it a `namespace { … }`
// body, an `extern "C"` block or a preprocessor branch would dispatch every C++
// construct inside through the C cases, which know nothing of classes,
// templates or member functions, and they would vanish silently rather than
// fail loudly.
//
// Name resolution is the other part. C's declaratorName stops at an identifier,
// so `void Foo::bar()`, `Foo::~Foo()` and `Foo &Foo::operator=()` — the shape
// most of a .cpp file consists of — all resolve to nothing under it, and C's
// findDeclarator does not look through a reference_declarator at all, so a
// reference-returning function is dropped before it is even named.
type cppWalk struct {
	cWalk
}

// cppScope is what one level of the walk hands its children: the enclosing
// symbol to attach containment to, the `::` prefix to qualify names with, and
// whether that enclosing symbol is a class body — which is the only thing that
// separates a method from a function and a field from a variable, since the two
// pairs are spelt identically.
type cppScope struct {
	parent  int64
	prefix  string
	inClass bool
}

// top handles one declaration. A compound_statement is never descended into,
// which is what suppresses locals: inside a function body a `declaration` looks
// exactly like a file-scope one.
func (w *cppWalk) top(n *tsg.Node, sc cppScope) {
	switch n.Type(w.lang) {
	case "preproc_include":
		w.addInclude(n)
	case "preproc_def":
		w.addMacro(n, topology.KindConstant)
	case "preproc_function_def":
		w.addMacro(n, topology.KindFunction)
	case "namespace_definition":
		w.addNamespace(n, sc)
	case "class_specifier", "struct_specifier", "union_specifier", "enum_specifier":
		w.addAggregate(n, n, sc)
	case "function_definition":
		w.addFunction(n, n, sc)
	case "declaration", "field_declaration":
		w.addDecl(n, n, sc)
	case "template_declaration":
		w.addTemplate(n, sc)
	case "alias_declaration", "type_definition", "concept_definition":
		w.addAlias(n, n, sc)
	case "preproc_if", "preproc_ifdef", "preproc_else", "preproc_elif",
		"linkage_specification", "declaration_list", "field_declaration_list", "ERROR":
		// Conditional compilation, `extern "C" { … }` and namespace bodies all
		// wrap ordinary declarations, and a class body is dispatched through
		// this same switch so a nested type, a member template and an inline
		// method need no cases of their own.
		//
		// ERROR is here for recall, not tidiness. Where the grammar mis-parses,
		// the recovery node routinely spans the rest of the file, but the
		// declarations inside it are still parsed as their own typed nodes;
		// descending collects those. It invents nothing — every symbol must
		// still come from a node this switch recognises.
		w.children(n, sc)
	default:
		// Under an ERROR the grammar's hidden repetition rules surface as
		// ordinary `…_repeat1` children, and the recovered declarations hang off
		// them rather than off the ERROR itself. Without this step the descent
		// above stops one level short and recovers nothing at all.
		if strings.HasSuffix(n.Type(w.lang), "_repeat1") {
			w.children(n, sc)
		}
	}
}

func (w *cppWalk) children(n *tsg.Node, sc cppScope) {
	for _, c := range n.Children() {
		w.top(c, sc)
	}
}

// emit appends one node, stamped with its byte span and any doc comment flush
// above it, and links it to its enclosing symbol. Routing every emission through
// one place is what makes "every node carries a span" structural rather than a
// rule each call site has to remember.
func (w *cppWalk) emit(kind topology.NodeKind, name, qualified, sig string, n *tsg.Node, parent int64) int64 {
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: qualified,
		Signature: sig,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  w.langName,
		Path:      w.path,
	}
	setSpan(&node, n)
	node.DocStartByte, node.DocEndByte = docSpanBefore(n, w.lang, w.src, isCComment)
	w.nodes = append(w.nodes, node)
	w.link(parent, idx)
	return idx
}

// addNamespace emits nothing and descends, carrying the namespace name into the
// prefix. An anonymous namespace contributes no prefix, which is right: it has
// no name anyone can write.
func (w *cppWalk) addNamespace(n *tsg.Node, sc cppScope) {
	if id := n.ChildByFieldName("name", w.lang); id != nil {
		sc.prefix = cppQualify(sc.prefix, id.Text(w.src))
	}
	if body := childByType(n, "declaration_list", w.lang); body != nil {
		for _, c := range body.Children() {
			w.top(c, cppScope{parent: sc.parent, prefix: sc.prefix})
		}
	}
}

// addAggregate emits a class, struct, union or enum and then its body. outer is
// the node the span is taken from — the enclosing template_declaration for a
// class template, so that replace_symbol_body and move_symbol slice the
// `template <…>` header along with the class it belongs to rather than leaving
// it stranded.
//
// An unnamed specifier is skipped here: it belongs to a typedef or to a member
// declaration, both of which emit it themselves.
func (w *cppWalk) addAggregate(n, outer *tsg.Node, sc cppScope) {
	id := childByType(n, "type_identifier", w.lang)
	if id == nil {
		return
	}
	name := id.Text(w.src)
	// `class` and `struct` differ only in default access, so both are classes.
	// A union is a storage layout and an enum a value set; neither carries the
	// class reading.
	kind := topology.KindClass
	switch n.Type(w.lang) {
	case "union_specifier", "enum_specifier":
		kind = topology.KindType
	}
	qualified := cppQualify(sc.prefix, name)
	idx := w.emit(kind, name, qualified, w.headText(outer, n), outer, sc.parent)
	w.addBody(n, idx, qualified)
}

// addBody emits whatever a type's body holds: class members go back through the
// top-level switch (a member is spelt exactly like a file-scope declaration, so
// one dispatch serves both), enumerators through their own emitter.
func (w *cppWalk) addBody(spec *tsg.Node, parent int64, prefix string) {
	if list := childByType(spec, "field_declaration_list", w.lang); list != nil {
		for _, c := range list.Children() {
			w.top(c, cppScope{parent: parent, prefix: prefix, inClass: true})
		}
	}
	if list := childByType(spec, "enumerator_list", w.lang); list != nil {
		w.addEnumerators(list, parent, prefix)
	}
}

// addEnumerators emits an enum's values as constants owned by the enum. This
// shadows cWalk.addEnumerators for the separator alone — C joins a member's
// qualified name with "." and C++ must use "::" — and needs none of C's
// three-enumerator recovery, because that gotreesitter defect does not reproduce
// in the C++ grammar.
func (w *cppWalk) addEnumerators(list *tsg.Node, parent int64, prefix string) {
	for _, e := range list.Children() {
		if e.Type(w.lang) != "enumerator" {
			continue
		}
		id := childByType(e, "identifier", w.lang)
		if id == nil {
			continue
		}
		name := id.Text(w.src)
		w.emit(topology.KindConstant, name, cppQualify(prefix, name), "", e, parent)
	}
}

// addFunction emits a definition: a free function, an inline member, or an
// out-of-line member definition.
func (w *cppWalk) addFunction(n, outer *tsg.Node, sc cppScope) {
	decl := w.funcDeclarator(n)
	if decl == nil {
		return
	}
	raw := w.nameOf(decl)
	if raw == "" {
		return
	}
	name, kind, qualified := lastSegment(raw), topology.KindFunction, cppQualify(sc.prefix, raw)
	switch test := w.testName(decl, raw); {
	case test != "":
		name, kind, qualified = test, topology.KindTest, test
	case sc.inClass || strings.Contains(raw, "::"):
		// `void Foo::bar()` at file scope is a member of Foo, not a free
		// function; the qualification is the only evidence of that, and it is
		// how most of a .cpp file's contents are written.
		kind = topology.KindMethod
	}
	idx := w.emit(kind, name, qualified, w.signature(outer, decl), outer, sc.parent)
	w.defined[name] = true
	// A definition displaces a same-named declaration in the call index rather
	// than deferring to it (noteFunc keeps the first): the call edges a body
	// produces belong on the node that has the body.
	w.funcIdx[name] = idx
}

// addDecl emits a declaration, which C++ uses for four unrelated things: a
// file-scope prototype, an in-class method or constructor declaration, a data
// member, and a variable. Whether the declarator eventually names a function is
// the whole test.
func (w *cppWalk) addDecl(n, outer *tsg.Node, sc cppScope) {
	if decl := w.funcDeclarator(n); decl != nil {
		w.addCallableDecl(n, outer, decl, sc)
		return
	}
	raw := w.declaredName(n)
	if raw == "" {
		return
	}
	name := lastSegment(raw)
	// Mutability decides the kind, in or out of a class. NOT KindField: that kind
	// is for a key of a data-format file (a SQL column, a TOML key), and
	// topology.KindField's own doc comment says a member of a *code* type is
	// KindConstant when immutable and KindVariable otherwise — which Java and
	// Kotlin follow and TestExtractors_MemberConventions enforces. Keying off
	// where the member sits rather than whether it is const put C++ outside that
	// convention, and the guard could not catch it because C++ was not in the
	// table.
	kind := topology.KindVariable
	if w.isConstDecl(n) {
		kind = topology.KindConstant
	}
	w.emit(kind, name, cppQualify(sc.prefix, raw), normaliseSpace(n.Text(w.src)), outer, sc.parent)
}

// addCallableDecl emits a declaration whose declarator names a function.
//
// Inside a class it is emitted straight away: a header's classes are most of a
// C++ API's surface, and holding a method back would risk a containment edge
// pointing at a node that never arrives. At file scope it goes through C's
// hold-back instead, which drops it if this file also defines it.
func (w *cppWalk) addCallableDecl(n, outer, decl *tsg.Node, sc cppScope) {
	if !sc.inClass {
		w.addPrototype(n, decl)
		return
	}
	raw := w.nameOf(decl)
	if raw == "" {
		return
	}
	name := lastSegment(raw)
	// The whole declaration, not signature()'s head slice: a declaration has no
	// body to trim off, and the tail is where `= 0`, `override` and `noexcept`
	// live.
	idx := w.emit(topology.KindMethod, name, cppQualify(sc.prefix, raw),
		normaliseSpace(n.Text(w.src)), outer, sc.parent)
	w.noteFunc(name, idx)
}

// addTemplate emits the declaration a `template <…>` header introduces, passing
// the header's node as the span so the two stay one symbol.
func (w *cppWalk) addTemplate(n *tsg.Node, sc cppScope) {
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "class_specifier", "struct_specifier", "union_specifier", "enum_specifier":
			w.addAggregate(c, n, sc)
		case "function_definition":
			w.addFunction(c, n, sc)
		case "declaration", "field_declaration":
			w.addDecl(c, n, sc)
		case "alias_declaration", "type_definition", "concept_definition":
			w.addAlias(c, n, sc)
		case "template_declaration":
			w.addTemplate(c, sc)
		}
	}
}

// addAlias emits `using X = T;`, `typedef T X;` and `concept C = …`. All three
// introduce a name that stands for something else, and a reader looks them up
// the same way, so they share a kind.
func (w *cppWalk) addAlias(n, outer *tsg.Node, sc cppScope) {
	name := ""
	if n.Type(w.lang) == "type_definition" {
		// `typedef struct { … } Point;` puts the introduced name last; the
		// earlier type_identifier, when there is one, is the aliased type.
		for _, c := range n.Children() {
			if c.Type(w.lang) == "type_identifier" {
				name = c.Text(w.src)
			}
		}
	} else if id := n.ChildByFieldName("name", w.lang); id != nil {
		name = id.Text(w.src)
	}
	if name == "" {
		return
	}
	qualified := cppQualify(sc.prefix, name)
	idx := w.emit(topology.KindType, name, qualified, normaliseSpace(n.Text(w.src)), outer, sc.parent)
	// A typedef of an anonymous aggregate is the C spelling and still appears in
	// C++ headers; its members would otherwise have no named parent to hang off.
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "struct_specifier", "union_specifier", "enum_specifier":
			w.addBody(c, idx, qualified)
		}
	}
}

// funcDeclarator returns the function_declarator a definition or declaration
// eventually reaches, looking through every wrapper C++ can put between them.
// The reference_declarator case is the one C does not have: `Foo &Foo::bar()`
// and `int &get()` are ordinary C++ and invisible without it.
func (w *cppWalk) funcDeclarator(n *tsg.Node) *tsg.Node {
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "function_declarator":
			return c
		case "pointer_declarator", "reference_declarator", "parenthesized_declarator",
			"init_declarator", "array_declarator":
			if d := w.funcDeclarator(c); d != nil {
				return d
			}
		}
	}
	return nil
}

// nameOf resolves a declarator subtree to the name it introduces, returning a
// qualified name whole (`Foo::bar`, `Foo::~Foo`, `Foo::operator=`) so the caller
// can decide what to do with the scope.
//
// It descends through the `declarator` FIELD rather than by matching child
// types, which is what keeps a type from being mistaken for a name: in
// `geom::Point p;` the type is a qualified_identifier sitting exactly where a
// name-shaped child search would find it first.
func (w *cppWalk) nameOf(d *tsg.Node) string {
	switch d.Type(w.lang) {
	case "identifier", "field_identifier", "type_identifier",
		"destructor_name", "operator_name", "qualified_identifier":
		return d.Text(w.src)
	case "template_function":
		if nm := d.ChildByFieldName("name", w.lang); nm != nil {
			return w.nameOf(nm)
		}
		return ""
	}
	if inner := d.ChildByFieldName("declarator", w.lang); inner != nil {
		return w.nameOf(inner)
	}
	// A reference_declarator holds its inner declarator as a plain child, with
	// no field label, so the field walk above cannot reach through one.
	for _, c := range d.Children() {
		if name := w.nameOf(c); name != "" {
			return name
		}
	}
	return ""
}

// declaredName returns the name a declaration introduces, via its declarator
// field so that a multi-word type is never read as the name.
func (w *cppWalk) declaredName(n *tsg.Node) string {
	d := n.ChildByFieldName("declarator", w.lang)
	if d == nil {
		return ""
	}
	return w.nameOf(d)
}

// isConstDecl reports whether a declaration introduces a constant. The prefix
// test covers `const`, `constexpr`, `constinit` and `consteval` in one rule.
func (w *cppWalk) isConstDecl(n *tsg.Node) bool {
	q := childByType(n, "type_qualifier", w.lang)
	return q != nil && strings.HasPrefix(q.Text(w.src), "const")
}

// headText returns a type's declaration head — everything from outer's start up
// to the body — collapsed to one line, so a class signature carries its
// template header, `final` and its base list without the members.
func (w *cppWalk) headText(outer, spec *tsg.Node) string {
	start, end := clampU32(outer.StartByte()), clampU32(spec.EndByte())
	for _, t := range []string{"field_declaration_list", "enumerator_list"} {
		if b := childByType(spec, t, w.lang); b != nil {
			end = clampU32(b.StartByte())
		}
	}
	if start >= end || end > len(w.src) {
		return ""
	}
	return normaliseSpace(string(w.src[start:end]))
}

// cppTestMacros are the test-defining macros whose bodies the grammar parses as
// an ordinary function_definition, which is what makes them recognisable at all.
//
// Catch2's TEST_CASE is deliberately absent: it takes string arguments, which
// the grammar cannot reconcile with a function declarator, so it does not parse
// as a definition — see the extractor's test for the measured shape.
var cppTestMacros = map[string]bool{
	"TEST": true, "TEST_F": true, "TEST_P": true,
	"TYPED_TEST": true, "TYPED_TEST_P": true, "TEST_CASE_METHOD": true,
	"BOOST_AUTO_TEST_CASE": true, "BOOST_FIXTURE_TEST_CASE": true,
}

// testName returns the name of a macro-defined test, or "" when decl is not one.
// The macro's arguments are joined with "." because that is the name the test
// runner itself uses: `SuiteName.DoesThing` is what --gtest_filter matches, so
// it is what someone searching for a failing test types.
func (w *cppWalk) testName(decl *tsg.Node, macro string) string {
	if !cppTestMacros[macro] {
		return ""
	}
	list := childByType(decl, "parameter_list", w.lang)
	if list == nil {
		return ""
	}
	var parts []string
	for _, p := range list.Children() {
		if p.Type(w.lang) == "parameter_declaration" {
			parts = append(parts, normaliseSpace(p.Text(w.src)))
		}
	}
	return strings.Join(parts, ".")
}

// symbolName returns the key a definition was indexed under, so the call-edge
// pass resolves an enclosing function the same way the emitting pass named it.
func (w *cppWalk) symbolName(decl *tsg.Node) string {
	raw := w.nameOf(decl)
	if tn := w.testName(decl, raw); tn != "" {
		return tn
	}
	return lastSegment(raw)
}

// callEdges attributes each call to the function whose body contains it. This
// shadows cWalk.callEdges because both halves of it — finding the enclosing
// definition's declarator and naming it — are the C resolution that C++ outgrows.
func (w *cppWalk) callEdges(root *tsg.Node) {
	w.nameCounts = callableNameCounts(w.nodes)
	seen := map[[2]int64]bool{}
	enclosing := func(n *tsg.Node, cur int64) int64 {
		if n.Type(w.lang) != "function_definition" {
			return cur
		}
		decl := w.funcDeclarator(n)
		if decl == nil {
			return cur
		}
		if idx, ok := w.funcIdx[w.symbolName(decl)]; ok {
			return idx
		}
		return cur
	}
	walkCallSites(root, enclosing, func(n *tsg.Node, cur int64) {
		if cur < 0 || n.Type(w.lang) != "call_expression" {
			return
		}
		target, ok := w.funcIdx[w.calleeName(n)]
		if !ok || target == cur {
			return
		}
		key := [2]int64{cur, target}
		if seen[key] {
			return
		}
		seen[key] = true
		w.edges = append(w.edges, heuristicCallEdge(cur, target, w.nodes, w.nameCounts))
	})
}

// calleeName returns the bare name a call expression targets. Only the final
// segment is used — `a.b()`, `A::b()` and `b<T>()` all resolve to `b` — because
// the index is keyed by name and tree-sitter carries no type information to tell
// two receivers apart; heuristicCallEdge down-weights the edge when the name is
// shared.
func (w *cppWalk) calleeName(call *tsg.Node) string {
	fn := call.ChildByFieldName("function", w.lang)
	if fn == nil {
		return ""
	}
	switch fn.Type(w.lang) {
	case "identifier":
		return fn.Text(w.src)
	case "qualified_identifier":
		return lastSegment(fn.Text(w.src))
	case "field_expression":
		if f := fn.ChildByFieldName("field", w.lang); f != nil {
			return f.Text(w.src)
		}
	case "template_function":
		if nm := fn.ChildByFieldName("name", w.lang); nm != nil {
			return nm.Text(w.src)
		}
	}
	return ""
}

// cppQualify joins a scope prefix to a name with C++'s scope operator. A name
// that is already qualified keeps its own scopes, so a member defined out of
// line inside a namespace reads `geom::Point::norm`.
func cppQualify(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "::" + name
}
