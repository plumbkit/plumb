package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// PHPExtractor extracts PHP symbols using the gotreesitter PHP grammar.
//
// Concurrency: stateless after construction and safe for concurrent use; each
// Extract call borrows a parser from the shared per-grammar pool and returns it,
// because gotreesitter parsers are not safe for concurrent reuse.
type PHPExtractor struct {
	lang lazyGrammar
}

// NewPHP returns a tree-sitter-backed PHP extractor.
func NewPHP() *PHPExtractor {
	return &PHPExtractor{lang: lazyGrammar{load: grammars.PhpLanguage}}
}

func (e *PHPExtractor) Language() string     { return "php" }
func (e *PHPExtractor) Extensions() []string { return []string{".php"} }

// Extract parses src and returns PHP's declarations: namespaces (KindPackage,
// as Go's extractor maps its package clause), classes, traits and enums
// (KindClass), interfaces (KindType — a contract, matching the Rust trait /
// Kotlin interface mapping), methods and functions, properties and class
// constants, enum cases, `use` imports in all four spellings, closures and
// arrow functions bound to a name, and PHPUnit tests — with certain (1.0)
// containment edges from each container to what it declares and name-resolved
// heuristic (0.8) call edges between callables in the same file.
//
// A PHP file is a TEMPLATE first: it may open in HTML, close `?>` and reopen
// `<?php` any number of times. The grammar parses each `?> … <?php` run as a
// `text_interpolation` node and leaves the declarations on either side as
// ordinary siblings of `program`, so a plain descent reaches every block.
//
// Returns (nil, nil, nil) when src cannot be parsed.
func (e *PHPExtractor) Extract(ctx context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	lang := e.lang.get()
	return extractWith(ctx, lang, src, func(root *tsg.Node) ([]topology.Node, []topology.Edge) {
		return phpWalkTree(root, lang, src, relPath, true)
	})
}

// phpWalkTree runs both passes over an already-parsed tree. errorDescent is the
// seam php_test.go's A/B flips: it is the only difference between the shipped
// walk and the naive one, so the recall ERROR recovery buys can be measured
// rather than asserted.
func phpWalkTree(root *tsg.Node, lang *tsg.Language, src []byte, path string, errorDescent bool) ([]topology.Node, []topology.Edge) {
	w := &phpWalk{lang: lang, src: src, path: path, funcIdx: map[string]int64{}, errorDescent: errorDescent}
	w.walk(root, phpCtx{parent: -1})
	w.callEdges(root)
	return w.nodes, w.edges
}

type phpWalk struct {
	lang         *tsg.Language
	src          []byte
	path         string
	nodes        []topology.Node
	edges        []topology.Edge
	funcIdx      map[string]int64 // callable name → node index, for call edges
	nameCounts   map[string]int   // callable Name → count, for the ambiguous-call down-weight (#30)
	errorDescent bool
}

// phpCtx is the state a level of the walk inherits.
//
// ns and owner are kept apart rather than folded into one prefix because PHP
// scopes them differently: a class belongs to its namespace, but a `function`
// declared inside a method body belongs to the NAMESPACE — PHP hoists it to
// global scope — so one combined prefix would give it a qualified name no PHP
// tool would ever print.
type phpCtx struct {
	parent    int64  // node index of the enclosing container, -1 at file scope
	ns        string // enclosing namespace, "" for the global namespace
	owner     string // qualified name of the enclosing class/interface/trait/enum
	inBody    bool   // inside a function or method body — locals are suppressed
	testClass bool   // the enclosing class is a PHPUnit test case
}

func (w *phpWalk) walk(n *tsg.Node, c phpCtx) {
	cur := c
	for _, ch := range n.Children() {
		switch ch.Type(w.lang) {
		case "namespace_definition":
			cur = w.addNamespace(ch, cur)
		case "namespace_use_declaration":
			w.addImports(ch)
		case "class_declaration", "trait_declaration", "enum_declaration":
			// A trait and an enum are KindClass because both carry
			// implementation, while an interface below is a contract with no
			// body — the concrete-versus-contract split rust.go, kotlin.go and
			// swift.go already draw.
			w.addType(ch, cur, topology.KindClass)
		case "interface_declaration":
			w.addType(ch, cur, topology.KindType)
		case "method_declaration", "function_definition":
			w.addCallable(ch, cur)
		case "property_declaration":
			if !cur.inBody {
				w.addMembers(ch, cur, "property_element", false)
			}
		case "const_declaration":
			if !cur.inBody {
				w.addConstants(ch, cur)
			}
		case "enum_case":
			// An enum case is a constant, as java.go treats an enum constant
			// and rust.go an enum variant.
			w.addConstant(ch, cur, w.declName(ch))
		case "expression_statement":
			w.addAssigned(ch, cur)
		case "ERROR":
			// Descend into a recovery node. One unbalanced brace makes the
			// grammar wrap the rest of the file in a single ERROR, yet the
			// classes and functions inside it are still parsed correctly as
			// their own typed nodes — descending recovers them without
			// guessing, since every symbol still comes from a real typed node
			// rather than from the ERROR's text. The hidden `…_repeat1`
			// repetition nodes gotreesitter surfaces beneath an ERROR fall to
			// the default arm, which descends through anything unrecognised.
			if w.errorDescent {
				w.walk(ch, cur)
			}
		default:
			w.walk(ch, cur)
		}
	}
}

// addNamespace emits a namespace as a KindPackage — the mapping Go's extractor
// already uses for its package clause — and returns the context applying after
// it. The two spellings differ in scope, which is why a context comes back at
// all: `namespace X { … }` governs only its braces and is walked as a child,
// while `namespace X;` governs the REST OF THE FILE, so its context must
// replace the caller's for every later sibling — hence walk's mutable cursor.
func (w *phpWalk) addNamespace(n *tsg.Node, c phpCtx) phpCtx {
	name := strings.TrimSpace(w.declName(n))
	body := childByType(n, "compound_statement", w.lang)
	if name == "" {
		// `namespace { … }` is the explicit global namespace: no symbol, but
		// its body still has to be walked.
		if body != nil {
			w.walk(body, phpCtx{parent: c.parent, ns: c.ns})
		}
		return c
	}
	idx := w.emit(n, c.parent, topology.KindPackage, name, name, "namespace "+name, true)
	if body != nil {
		w.walk(body, phpCtx{parent: idx, ns: name})
		return c
	}
	return phpCtx{parent: idx, ns: name}
}

// addImports emits one import per `use` clause, across all four spellings PHP
// accepts: plain, aliased, grouped, and `use function` / `use const`. A group
// becomes several nodes because `use App\Model\{Order, Item}` really is two
// dependencies, and one node named after the prefix would hide the second.
func (w *phpWalk) addImports(n *tsg.Node) {
	prefix, clauses := "", n
	if group := childByType(n, "namespace_use_group", w.lang); group != nil {
		clauses = group
		if p := childByType(n, "namespace_name", w.lang); p != nil {
			prefix = strings.TrimSpace(p.Text(w.src)) + `\`
		}
	}
	for _, clause := range clauses.Children() {
		if clause.Type(w.lang) != "namespace_use_clause" {
			continue
		}
		// The imported path is the clause's FIRST named child: an alias
		// (`use A\B as C`) contributes a SECOND bare `name`, and naming the node
		// after the alias would break a search for the real dependency. The
		// clause text kept as the signature preserves the alias and the
		// `function` / `const` keyword for anyone who needs them.
		target := firstNamedChild(clause)
		if target == nil {
			continue
		}
		path := prefix + strings.TrimSpace(target.Text(w.src))
		if path == prefix {
			continue
		}
		// The span is the CLAUSE, not the statement, so group members get
		// distinct, non-overlapping spans; two nodes sharing one span make an
		// edit range ambiguous for whichever consumer resolves it first.
		w.emit(clause, -1, topology.KindImport, path, path, normaliseSpace(clause.Text(w.src)), false)
	}
}

// addType emits a class, trait, enum or interface and walks its body.
func (w *phpWalk) addType(n *tsg.Node, c phpCtx, kind topology.NodeKind) {
	name := w.declName(n)
	if name == "" {
		w.walk(n, c)
		return
	}
	qualified := phpJoinNS(c.ns, name)
	idx := w.emit(n, c.parent, kind, name, qualified, w.signatureHead(n, "declaration_list", "enum_declaration_list"), true)
	for _, list := range []string{"declaration_list", "enum_declaration_list"} {
		if body := childByType(n, list, w.lang); body != nil {
			w.walk(body, phpCtx{parent: idx, ns: c.ns, owner: qualified, testClass: w.isTestClass(n, name)})
			return
		}
	}
}

// addCallable emits a `function` or a method. A method_declaration outside any
// type — which only happens inside an ERROR recovery node — is emitted as a
// function, matching how java.go decides the same case.
func (w *phpWalk) addCallable(n *tsg.Node, c phpCtx) {
	name := w.declName(n)
	if name == "" {
		return
	}
	kind := topology.KindFunction
	qualified := phpJoinNS(c.ns, name)
	if n.Type(w.lang) == "method_declaration" && c.owner != "" {
		kind, qualified = topology.KindMethod, c.owner+"::"+name
	}
	if w.isTestMethod(n, name, c.testClass) {
		kind = topology.KindTest
	}
	idx := w.emit(n, c.parent, kind, name, qualified, w.signatureHead(n, "compound_statement"), true)
	w.remember(name, idx)
	if name == "__construct" {
		w.addPromoted(n, c)
	}
	if body := childByType(n, "compound_statement", w.lang); body != nil {
		// ns and owner carry through: a class or function declared inside a
		// method body is not a member of that method, and only the parent link
		// says where it was written.
		w.walk(body, phpCtx{parent: idx, ns: c.ns, owner: c.owner, inBody: true, testClass: c.testClass})
	}
}

// addPromoted emits PHP 8 constructor property promotion. The parameters of
// `__construct(private readonly string $svc)` are not parameters at all — they
// declare properties on the class — so they attach to the enclosing type, not
// to the constructor that spells them, and each carries its OWN modifiers.
func (w *phpWalk) addPromoted(ctor *tsg.Node, c phpCtx) {
	if params := childByType(ctor, "formal_parameters", w.lang); params != nil {
		w.addMembers(params, c, "property_promotion_parameter", true)
	}
}

// addMembers emits each property element of n, so `public int $a = 1, $b = 2;`
// yields two symbols rather than one. ownMods says whether the element carries
// its own modifiers (a promoted constructor parameter) or shares the
// declaration's (an ordinary property).
//
// `readonly` is what makes a property immutable in PHP, so it decides
// KindConstant versus KindVariable — the cross-language member convention
// documented on topology.KindField and enforced by
// TestExtractors_MemberConventions. Deliberately NOT KindField: that kind is
// reserved for a key/column of a DATA-FORMAT file.
//
// The `$` sigil is dropped from the Name because it belongs to the variable
// spelling, not to the property — the same member is read as `$obj->svc` — so
// Name matches how the property is used while Qualified keeps PHP's own
// `Class::$prop` notation.
func (w *phpWalk) addMembers(n *tsg.Node, c phpCtx, elemType string, ownMods bool) {
	for _, el := range n.Children() {
		nm := el.ChildByFieldName("name", w.lang)
		if el.Type(w.lang) != elemType || nm == nil {
			continue
		}
		name := strings.TrimPrefix(strings.TrimSpace(nm.Text(w.src)), "$")
		if name == "" {
			continue
		}
		mods := n
		if ownMods {
			mods = el
		}
		kind := topology.KindVariable
		if childByType(mods, "readonly_modifier", w.lang) != nil {
			kind = topology.KindConstant
		}
		qualified := name
		if c.owner != "" {
			qualified = c.owner + "::$" + name
		}
		w.emit(el, c.parent, kind, name, qualified, normaliseSpace(mods.Text(w.src)), false)
	}
}

// addConstants emits each element of a `const` declaration, at class scope or
// at file scope — PHP allows both, and a file-scope `const` is the modern
// replacement for `define()`.
func (w *phpWalk) addConstants(n *tsg.Node, c phpCtx) {
	for _, el := range n.Children() {
		if el.Type(w.lang) == "const_element" {
			if nm := firstNamedChild(el); nm != nil {
				w.addConstant(el, c, nm.Text(w.src))
			}
		}
	}
}

// addConstant emits a named constant qualified `Class::NAME`, PHP's own
// notation for one, falling back to the namespace path at file scope.
func (w *phpWalk) addConstant(n *tsg.Node, c phpCtx, name string) {
	if name == "" {
		return
	}
	qualified := phpJoinNS(c.ns, name)
	if c.owner != "" {
		qualified = c.owner + "::" + name
	}
	w.emit(n, c.parent, topology.KindConstant, name, qualified, normaliseSpace(n.Text(w.src)), false)
}

// addAssigned emits a closure or arrow function bound to a name, which is how
// PHP declares a callable outside a class without the `function` keyword.
//
// Only a binding OUTSIDE a function body is emitted. A `$fn = fn() => …` inside
// a method is a local — PHP code is full of them as inline callbacks — and
// emitting those would bury the declarations, the locals-suppression rule every
// other extractor here applies. The body is descended with inBody set, so a
// closure nested in a file-scope closure is a local too.
func (w *phpWalk) addAssigned(n *tsg.Node, c phpCtx) {
	assign := childByType(n, "assignment_expression", w.lang)
	var lhs, fn *tsg.Node
	if assign != nil && !c.inBody {
		lhs = firstNamedChild(assign)
		for _, t := range []string{"anonymous_function", "arrow_function"} {
			if f := childByType(assign, t, w.lang); f != nil {
				fn = f
			}
		}
	}
	if fn == nil || lhs == nil || lhs.Type(w.lang) != "variable_name" {
		w.walk(n, c)
		return
	}
	name := strings.TrimPrefix(strings.TrimSpace(lhs.Text(w.src)), "$")
	if name == "" {
		w.walk(n, c)
		return
	}
	idx := w.emit(assign, c.parent, topology.KindFunction, name, phpJoinNS(c.ns, name), name+" = "+w.signatureHead(fn, "compound_statement"), true)
	w.remember(name, idx)
	w.walk(fn, phpCtx{parent: idx, ns: c.ns, owner: c.owner, inBody: true, testClass: c.testClass})
}

// remember records the first definition of a callable name — first wins, as in
// the other extractors, so an ambiguous call is down-weighted rather than aimed
// at an arbitrary redefinition.
func (w *phpWalk) remember(name string, idx int64) {
	if _, seen := w.funcIdx[name]; !seen {
		w.funcIdx[name] = idx
	}
}

// emit appends a node, stamps its byte span, and links it to its container with
// a certain (1.0/extractor) containment edge. withDoc is set only for the
// declaration kinds a doc comment can precede.
func (w *phpWalk) emit(n *tsg.Node, parent int64, kind topology.NodeKind, name, qualified, sig string, withDoc bool) int64 {
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: qualified,
		Signature: sig,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "php",
		Path:      w.path,
	}
	setSpan(&node, n)
	if withDoc {
		node.DocStartByte, node.DocEndByte = docSpanBefore(n, w.lang, w.src, isPHPComment)
	}
	w.nodes = append(w.nodes, node)
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

func (w *phpWalk) declName(n *tsg.Node) string {
	if nm := n.ChildByFieldName("name", w.lang); nm != nil {
		return nm.Text(w.src)
	}
	return ""
}

// signatureHead returns a declaration's head: the source text from the first
// non-attribute child up to its body. Taking the text as written rather than
// rebuilding it keeps modifiers, `extends`/`implements` clauses, default
// parameter values and union/nullable return types exactly as spelled, and
// spares this extractor a model of PHP's type grammar — where `?string`,
// `int|string|null` and `A&B` are three different nodes.
func (w *phpWalk) signatureHead(n *tsg.Node, bodyTypes ...string) string {
	start, end := clampU32(n.StartByte()), clampU32(n.EndByte())
	if a := childByType(n, "attribute_list", w.lang); a != nil {
		start = clampU32(a.EndByte()) // an attribute is metadata, not the head
	}
	for _, t := range bodyTypes {
		if b := childByType(n, t, w.lang); b != nil {
			end = clampU32(b.StartByte())
			break
		}
	}
	if end <= start || end > len(w.src) {
		return ""
	}
	return strings.TrimRight(normaliseSpace(string(w.src[start:end])), ";")
}

// isTestClass reports whether a class is a PHPUnit test case, by the two marks
// PHPUnit itself relies on: extending a *TestCase base, or being named *Test
// (which is what a phpunit.xml suite glob matches).
func (w *phpWalk) isTestClass(n *tsg.Node, name string) bool {
	if strings.HasSuffix(name, "Test") {
		return true
	}
	base := childByType(n, "base_clause", w.lang)
	return base != nil && strings.HasSuffix(strings.TrimSpace(base.Text(w.src)), "TestCase")
}

// isTestMethod recognises a PHPUnit test by all three marks the framework
// accepts: the `test` name prefix, the `#[Test]` attribute (PHPUnit 10+), and
// the `@test` docblock annotation it replaced.
//
// The name rule is deliberately narrower OUTSIDE a test class than PHPUnit's
// own, which takes any name beginning with `test` and would also claim
// `testable()` in ordinary code. Inside a plain test case that risk is absent,
// so the bare prefix is accepted there; elsewhere the next character must show
// that `test` is a whole word.
func (w *phpWalk) isTestMethod(n *tsg.Node, name string, inTestClass bool) bool {
	if w.hasTestAttribute(n) || strings.Contains(w.docText(n), "@test") {
		return true
	}
	if !strings.HasPrefix(name, "test") {
		return false
	}
	rest := name[len("test"):]
	if inTestClass || rest == "" {
		return true
	}
	return rest[0] == '_' || (rest[0] >= 'A' && rest[0] <= 'Z') || (rest[0] >= '0' && rest[0] <= '9')
}

// hasTestAttribute reports whether a declaration carries `#[Test]`. The simple
// name is compared, so the fully-qualified
// `#[PHPUnit\Framework\Attributes\Test]` matches too, and an attribute's
// argument list is stripped before the comparison.
func (w *phpWalk) hasTestAttribute(n *tsg.Node) bool {
	list := childByType(n, "attribute_list", w.lang)
	if list == nil {
		return false
	}
	for _, group := range list.Children() {
		for _, attr := range group.Children() {
			if attr.Type(w.lang) != "attribute" {
				continue
			}
			text := strings.TrimSpace(attr.Text(w.src))
			if i := strings.IndexByte(text, '('); i >= 0 {
				text = strings.TrimSpace(text[:i])
			}
			if phpLastSegment(text) == "Test" {
				return true
			}
		}
	}
	return false
}

// docText returns the text of the doc comment preceding n, used only to spot a
// `@test` annotation; the span itself is stamped by emit.
func (w *phpWalk) docText(n *tsg.Node) string {
	start, end := docSpanBefore(n, w.lang, w.src, isPHPComment)
	if end <= start || end > len(w.src) {
		return ""
	}
	return string(w.src[start:end])
}

// callEdges is the second pass: intra-file call edges attributed to the
// innermost enclosing function or method.
func (w *phpWalk) callEdges(root *tsg.Node) {
	w.nameCounts = callableNameCounts(w.nodes)
	seen := map[[2]int64]bool{}
	walkCallSites(root,
		scopeByType(w.lang, w.funcIdx, w.declName, "function_definition", "method_declaration"),
		func(n *tsg.Node, cur int64) {
			if cur < 0 {
				return
			}
			to, ok := w.funcIdx[w.calleeName(n)]
			if !ok || to == cur {
				return
			}
			key := [2]int64{cur, to}
			if seen[key] {
				return
			}
			seen[key] = true
			w.edges = append(w.edges, heuristicCallEdge(cur, to, w.nodes, w.nameCounts))
		})
}

// calleeName returns the simple name a call site invokes, across the four call
// forms PHP has: a plain call, `$this->m()`, `$obj?->m()` and `Class::m()`. The
// receiver is discarded because the topology call graph carries no type
// information — which is what heuristicCallEdge's ambiguity down-weight
// accounts for.
func (w *phpWalk) calleeName(n *tsg.Node) string {
	switch n.Type(w.lang) {
	case "function_call_expression":
		fn := n.ChildByFieldName("function", w.lang)
		if fn == nil {
			return ""
		}
		switch fn.Type(w.lang) {
		case "name":
			return fn.Text(w.src)
		case "qualified_name":
			return phpLastSegment(fn.Text(w.src))
		}
	case "member_call_expression", "nullsafe_member_call_expression", "scoped_call_expression":
		if nm := n.ChildByFieldName("name", w.lang); nm != nil {
			return nm.Text(w.src)
		}
	}
	return ""
}

// phpJoinNS qualifies a declaration with its namespace using PHP's own
// separator, so `App\Service\Invoice` is what a search — and a `use` statement
// elsewhere in the codebase — will contain.
func phpJoinNS(ns, name string) string {
	if ns == "" {
		return name
	}
	return ns + `\` + name
}

// phpLastSegment reduces a namespaced name to its simple name.
func phpLastSegment(name string) string {
	if i := strings.LastIndex(name, `\`); i >= 0 {
		return name[i+1:]
	}
	return name
}

// isPHPComment names the grammar's comment node. PHP has three comment
// syntaxes — `//`, `#` and `/* */` (of which `/** */` is the docblock) — and
// the grammar spells all three `comment`, so one case covers them.
func isPHPComment(typ string) bool { return typ == "comment" }
