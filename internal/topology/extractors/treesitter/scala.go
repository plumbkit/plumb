package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// ScalaExtractor extracts Scala symbols using the gotreesitter Scala grammar.
//
// Concurrency: stateless after construction and safe for concurrent use; a
// fresh parser is created per Extract call because gotreesitter parsers are not
// safe for concurrent reuse.
type ScalaExtractor struct {
	lang lazyGrammar
}

// NewScala returns a tree-sitter-backed Scala extractor.
func NewScala() *ScalaExtractor {
	return &ScalaExtractor{lang: lazyGrammar{load: grammars.ScalaLanguage}}
}

func (e *ScalaExtractor) Language() string     { return "scala" }
func (e *ScalaExtractor) Extensions() []string { return []string{".scala", ".sc"} }

// Extract parses src and returns Scala's declarations: packages (flat, braced
// and package objects), imports, classes / case classes / objects / traits /
// enums, defs, vals and vars, type aliases, Scala 3 givens and extension
// methods, and the cases a ScalaTest or munit suite declares — plus containment
// and intra-file call edges.
//
// The walk is STATEMENT-ORIENTED: it descends only into declaration bodies
// (template_body, with_template_body, enum_body), never into arbitrary
// expressions. That suppresses locals for free — a `def` or `val` inside a
// method body lands in a `block` (Scala 2) or an `indented_block` (Scala 3),
// neither of which the walk enters — and it drops the members of an
// anonymous/structural instance (`new Runnable { def run() = … }`), whose body is
// likewise a `block`: those belong to a type with no name, so there is nothing to
// navigate to.
//
// The qualified name carries the PACKAGE, since Scala's flat `package a.b` header
// applies to everything after it in the file: a top-level class indexes under the
// name people write (`cats.effect.IO`), and braced packages and package objects
// fall out of the same mechanism as any container.
func (e *ScalaExtractor) Extract(ctx context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	lang := e.lang.get()
	return extractWith(ctx, lang, src, func(root *tsg.Node) ([]topology.Node, []topology.Edge) {
		if root == nil {
			return nil, nil
		}
		w := &scalaWalk{lang: lang, src: src, path: relPath, funcIdx: map[string]int64{}, recover: true}
		w.walkBody(root, -1, "")
		w.callEdges(root)
		return w.nodes, w.edges
	})
}

type scalaWalk struct {
	lang       *tsg.Language
	src        []byte
	path       string
	nodes      []topology.Node
	edges      []topology.Edge
	funcIdx    map[string]int64
	nameCounts map[string]int
	// recover controls the descent into ERROR nodes and the hidden nodes beneath
	// them; a field only so a corpus A/B can turn it off. Production runs it on.
	recover bool
}

// walkBody iterates the statements of a container body, or the compilation unit
// itself. The prefix is threaded through the loop rather than fixed up front
// because a flat `package` clause changes it for the statements after it.
func (w *scalaWalk) walkBody(n *tsg.Node, parent int64, prefix string) {
	for _, c := range n.Children() {
		typ := c.Type(w.lang)
		if typ == "package_clause" {
			prefix = w.addPackage(c, parent, prefix)
			continue
		}
		if !w.typeDecl(typ, c, parent, prefix) {
			w.memberDecl(typ, c, parent, prefix)
		}
	}
}

// typeDecl handles the declarations that open a scope, reporting whether it
// recognised the node. The kinds follow the concrete-vs-contract convention the
// Rust, Kotlin and Swift extractors use: `class`, `case class`, `object`, `case
// object`, `enum` and a package object carry an implementation and are
// KindClass; only a pure `trait` is a KindType (see traitKind).
func (w *scalaWalk) typeDecl(typ string, c *tsg.Node, parent int64, prefix string) bool {
	switch typ {
	case "class_definition", "object_definition", "package_object", "enum_definition":
		w.addContainer(c, parent, prefix, topology.KindClass)
	case "trait_definition":
		w.addContainer(c, parent, prefix, w.traitKind(c))
	case "given_definition":
		w.addContainer(c, parent, prefix, topology.KindConstant)
	case "extension_definition":
		w.addExtension(c, parent, prefix)
	default:
		return false
	}
	return true
}

// memberDecl handles the declarations that do not open a scope, plus the descents
// that are not declarations: an enum's case list, a swallowed self-type body, and
// the recovery nodes an unparseable region leaves behind.
func (w *scalaWalk) memberDecl(typ string, c *tsg.Node, parent int64, prefix string) {
	switch typ {
	case "function_definition", "function_declaration":
		w.addFunc(c, parent, prefix)
	case "val_definition", "val_declaration":
		w.addBinding(c, parent, prefix, topology.KindConstant)
	case "var_definition", "var_declaration":
		w.addBinding(c, parent, prefix, topology.KindVariable)
	case "type_definition":
		w.addSimple(c, parent, prefix, topology.KindType)
	case "import_declaration":
		w.addImport(c)
	case "simple_enum_case", "full_enum_case":
		w.addSimple(c, parent, prefix, topology.KindConstant)
	case "enum_case_definitions":
		w.walkBody(c, parent, prefix)
	case "call_expression", "infix_expression":
		if body := w.selfTypeBody(c); body != nil {
			w.walkBody(body, parent, prefix)
			return
		}
		w.testStatement(c, parent, prefix)
	default:
		// Keep walking through a recovery node: the declarations inside one are
		// still parsed correctly as their own typed nodes, so descending collects
		// them without inventing anything. Beneath an ERROR gotreesitter also
		// surfaces the grammar's hidden nodes (`_block`, `_indent`, `…_repeat1`)
		// as ordinary children with the recovered declarations hanging off THOSE,
		// so a `_`-prefixed node must be descended too. Hidden nodes appear only
		// in a recovered tree, so this never widens the walk on a healthy file.
		if w.recover && (c.IsError() || strings.HasPrefix(typ, "_")) {
			w.walkBody(c, parent, prefix)
		}
	}
}

// addPackage records a package clause and returns the prefix now in force. Braced
// `package a.b { … }` has a body, so its contents are genuinely CONTAINED by it;
// flat `package a.b` owns nothing, since making the whole file its child would
// claim every import and class is nested inside a mere header.
func (w *scalaWalk) addPackage(n *tsg.Node, parent int64, prefix string) string {
	name := ""
	if id := childByType(n, "package_identifier", w.lang); id != nil {
		name = normaliseSpace(id.Text(w.src))
	}
	if name == "" {
		return prefix
	}
	idx := w.emit(n, parent, prefix, topology.KindPackage, name, "package "+name)
	qualified := w.nodes[idx].Qualified
	if body := w.bodyOf(n); body != nil {
		w.walkBody(body, idx, qualified)
		return prefix
	}
	return qualified
}

// addContainer emits a declaration that opens a scope — class, object, trait,
// enum, package object or `given` — then walks its body so members are
// attributed to it. A `given` stays a KindConstant even in the `given … with`
// form: it is an immutable instance binding. An anonymous one is named after the
// type it provides, there being no other handle on it.
func (w *scalaWalk) addContainer(n *tsg.Node, parent int64, prefix string, kind topology.NodeKind) {
	name := w.declName(n)
	if name == "" {
		return
	}
	idx := w.emit(n, parent, prefix, kind, name, w.headOf(n, "template_body", "with_template_body", "enum_body", "="))
	if body := w.bodyOf(n); body != nil {
		w.walkBody(body, idx, w.nodes[idx].Qualified)
	}
}

// addExtension emits the methods of a Scala 3 `extension` block. The block itself
// has no name so is not emitted, but its receiver becomes the methods' qualified
// prefix: `extension (s: String) def shout` indexes as `String.shout`, which is
// what the call site reads like.
func (w *scalaWalk) addExtension(n *tsg.Node, parent int64, prefix string) {
	recv := ""
	if p := childByType(n, "parameters", w.lang); p != nil {
		if p = childByType(p, "parameter", w.lang); p != nil {
			if t := childByType(p, "type_identifier", w.lang); t != nil {
				recv = t.Text(w.src)
			}
		}
	}
	// Always KindMethod: an extension is invoked on a receiver, never as a free
	// function, whatever scope it is written at.
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "function_definition", "function_declaration":
			if name := w.declName(c); name != "" {
				w.emit(c, parent, joinScala(prefix, recv), topology.KindMethod, name, w.headOf(c, "=", "block"))
			}
		}
	}
}

// addFunc emits a `def`. One declared inside a type is a method; one at file or
// package scope (Scala 3 allows top-level defs) is a function.
func (w *scalaWalk) addFunc(n *tsg.Node, parent int64, prefix string) {
	kind := topology.KindFunction
	if parent >= 0 && w.nodes[parent].Kind != topology.KindPackage {
		kind = topology.KindMethod
	}
	if name := w.declName(n); name != "" {
		w.emit(n, parent, prefix, kind, name, w.headOf(n, "=", "block", "indented_block"))
	}
}

// addBinding emits a `val` (KindConstant) or `var` (KindVariable). Scala draws
// the immutable/mutable line at the keyword, exactly the distinction
// topology.KindField's doc comment asks a code member to be sorted by — and
// KindField is reserved for the keys of a data-format file, so no Scala binding
// is ever one, at any scope.
func (w *scalaWalk) addBinding(n *tsg.Node, parent int64, prefix string, kind topology.NodeKind) {
	id := childByType(n, "identifier", w.lang)
	if id == nil {
		// A destructuring binding (`val (a, b) = pair`) has a pattern where the
		// name would be; the grammar hands over no identifier there to emit.
		return
	}
	w.emit(n, parent, prefix, kind, id.Text(w.src), w.headOf(n, "="))
}

// addSimple emits a declaration with no body: a `type` alias or a Scala 3 enum
// case. Both case forms — bare `case Red` and parameterised `case Leaf(value: A)`
// — are KindConstant, a case being a named variant of its enum; splitting one
// construct across two kinds would buy no navigation.
func (w *scalaWalk) addSimple(n *tsg.Node, parent int64, prefix string, kind topology.NodeKind) {
	if name := w.declName(n); name != "" {
		w.emit(n, parent, prefix, kind, name, normaliseSpace(n.Text(w.src)))
	}
}

// addImport records one import, keeping its whole path — selectors, renames and
// wildcards included — as the name. One node per declaration, not per selector:
// the list is still tokenised into the search index, and splitting it would give
// several nodes the same few bytes of span.
func (w *scalaWalk) addImport(n *tsg.Node) {
	text := normaliseSpace(n.Text(w.src))
	name := strings.TrimSpace(strings.TrimPrefix(text, "import"))
	if name == "" {
		return
	}
	node := topology.Node{
		Kind:      topology.KindImport,
		Name:      name,
		Qualified: name,
		Signature: text,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "scala",
		Path:      w.path,
	}
	setSpan(&node, n)
	w.nodes = append(w.nodes, node)
}

func (w *scalaWalk) emit(n *tsg.Node, parent int64, prefix string, kind topology.NodeKind, name, sig string) int64 {
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: joinScala(prefix, name),
		Signature: sig,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "scala",
		Path:      w.path,
	}
	setSpan(&node, n)
	node.DocStartByte, node.DocEndByte = docSpanBefore(n, w.lang, w.src, isScalaComment)
	w.nodes = append(w.nodes, node)
	if _, seen := w.funcIdx[name]; !seen && (kind == topology.KindFunction || kind == topology.KindMethod) {
		w.funcIdx[name] = idx
	}
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

// traitKind decides whether a trait is a contract or an implementation. One whose
// body holds a concrete member is a mixin carrying behaviour (KindClass); one that
// only declares, or has no body, is the Scala equivalent of a Rust trait or a
// Kotlin interface (KindType).
func (w *scalaWalk) traitKind(n *tsg.Node) topology.NodeKind {
	if body := w.bodyOf(n); body != nil && w.hasConcreteMember(body) {
		return topology.KindClass
	}
	return topology.KindType
}

func (w *scalaWalk) hasConcreteMember(body *tsg.Node) bool {
	for _, c := range body.Children() {
		switch c.Type(w.lang) {
		case "function_definition", "val_definition", "var_definition", "given_definition":
			return true
		case "call_expression":
			if inner := w.selfTypeBody(c); inner != nil && w.hasConcreteMember(inner) {
				return true
			}
		}
	}
	return false
}

// bodyOf returns the node holding a declaration's members, whichever of the
// three spellings was used. Scala 3's indentation syntax needs no special case:
// `class C:` with an indented body produces the same template_body as the braced
// form, with a `:` where the `{` would be.
func (w *scalaWalk) bodyOf(n *tsg.Node) *tsg.Node {
	for _, typ := range [...]string{"template_body", "with_template_body", "enum_body"} {
		if b := childByType(n, typ, w.lang); b != nil {
			return b
		}
	}
	return nil
}

// declName returns a declaration's name. `identifier` covers class, object,
// trait, enum, def and package object; the type-node fallbacks name the
// declarations the grammar spells with one, unwrapping a generic so an anonymous
// `given Ordering[String]` is named `Ordering`.
func (w *scalaWalk) declName(n *tsg.Node) string {
	if id := childByType(n, "identifier", w.lang); id != nil {
		return id.Text(w.src)
	}
	if t := childByType(n, "type_identifier", w.lang); t != nil {
		return t.Text(w.src)
	}
	if g := childByType(n, "generic_type", w.lang); g != nil {
		if t := childByType(g, "type_identifier", w.lang); t != nil {
			return t.Text(w.src)
		}
	}
	return ""
}

// headOf returns a declaration's text up to the first of stops, so a signature
// keeps its type parameters, parameter lists, `extends` clause and result type
// without the body behind them.
func (w *scalaWalk) headOf(n *tsg.Node, stops ...string) string {
	end := n.EndByte()
	for _, typ := range stops {
		if c := childByType(n, typ, w.lang); c != nil && c.StartByte() < end {
			end = c.StartByte()
		}
	}
	lo, hi := clampU32(n.StartByte()), clampU32(end)
	if lo >= hi || hi > len(w.src) {
		return normaliseSpace(n.Text(w.src))
	}
	return normaliseSpace(string(w.src[lo:hi]))
}

// testStatement emits a test case and any case nested inside it. Scala tests are
// not declarations — `test("adds") { … }` is a call taking a by-name block and
// `"A Stack" should "pop" in { … }` an infix expression, so the declaration walk
// sees neither, yet they are the only symbols a spec file navigates by.
func (w *scalaWalk) testStatement(n *tsg.Node, parent int64, prefix string) {
	kind, name, body := w.callTest(n)
	if n.Type(w.lang) == "infix_expression" {
		kind, name, body = w.infixTest(n)
	}
	if name == "" {
		return
	}
	idx := w.emit(n, parent, prefix, kind, name, w.headOf(n, "block", "indented_block"))
	if body != nil {
		w.testsIn(body, idx, w.nodes[idx].Qualified)
	}
}

// testsIn descends one test block, nesting a case under the group enclosing it.
// Only test shapes are recognised: a `val` or `def` in a spec block is a fixture
// local, and the locals rule still holds.
func (w *scalaWalk) testsIn(n *tsg.Node, parent int64, prefix string) {
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "call_expression", "infix_expression":
			w.testStatement(c, parent, prefix)
		}
	}
}

// infixTest matches the FlatSpec / WordSpec English forms — `"A Stack" should
// "pop values" in { … }` and `"A Stack" should { … }`. The trailing block is what
// makes the match safe: `x should be(3)` is the same operator over a call, so
// requiring a block keeps assertions out.
func (w *scalaWalk) infixTest(n *tsg.Node) (topology.NodeKind, string, *tsg.Node) {
	named := make([]*tsg.Node, 0, 3)
	for _, c := range n.Children() {
		if c.IsNamed() {
			named = append(named, c)
		}
	}
	if len(named) != 3 || named[1].Type(w.lang) != "identifier" || !w.isBlock(named[2]) {
		return "", "", nil
	}
	kind := topology.KindSection
	switch named[1].Text(w.src) {
	case "in":
		kind = topology.KindTest
	case "should", "must", "can", "when", "that":
	default:
		return "", "", nil
	}
	name := strings.TrimSpace(strings.ReplaceAll(normaliseSpace(named[0].Text(w.src)), `"`, ""))
	return kind, name, named[2]
}

// callTest matches the call forms: `test("name") { … }` and
// `describe("…") { it("…") { … } }`.
func (w *scalaWalk) callTest(n *tsg.Node) (topology.NodeKind, string, *tsg.Node) {
	head := firstNamedChild(n)
	for i := 0; head != nil && head.Type(w.lang) == "call_expression" && i < 4; i++ {
		head = firstNamedChild(head)
	}
	if head == nil || head.Type(w.lang) != "identifier" {
		return "", "", nil
	}
	kind, ok := scalaTestCall(head.Text(w.src))
	if !ok {
		return "", "", nil
	}
	name := w.firstStringArg(n)
	if name == "" {
		return "", "", nil
	}
	var body *tsg.Node
	if kids := n.Children(); len(kids) > 0 && w.isBlock(kids[len(kids)-1]) {
		body = kids[len(kids)-1]
	}
	return kind, name, body
}

// scalaTestCall names the runners worth indexing. `describe`, `feature` and
// `suite` organise, so they become sections the cases hang off.
func scalaTestCall(callee string) (topology.NodeKind, bool) {
	switch callee {
	case "describe", "feature", "suite":
		return topology.KindSection, true
	case "test", "it", "they", "property", "scenario", "example", "ignore":
		return topology.KindTest, true
	}
	return "", false
}

// firstStringArg returns the description a runner was given. The block is not
// searched: a string inside the case body is not its name.
func (w *scalaWalk) firstStringArg(n *tsg.Node) string {
	if w.isBlock(n) {
		return ""
	}
	if n.Type(w.lang) == "string" {
		return strings.TrimSpace(strings.Trim(strings.TrimSpace(n.Text(w.src)), `"`))
	}
	for _, c := range n.Children() {
		if found := w.firstStringArg(c); found != "" {
			return found
		}
	}
	return ""
}

// selfTypeBody recovers a trait body the grammar swallowed into a call.
//
// The defect: a self-type followed by a DOC COMMENT loses to Scala 3's
// fewer-braces colon-argument rule, so `trait C { this: Informing =>` +
// `/** doc */` + `def h()` parses as call_expression(`this`, colon_argument(…
// indented_block(…))) rather than self_type + function_definition; remove the
// comment and the same file parses correctly. Idiomatic Scala documents its
// methods, so this is no corner — scalatest's GivenWhenThen indexed with none of
// its methods. The recovery is narrow: it fires only for a colon_argument holding
// an indented block whose head text still carries the self-type's `=>`, so an
// ordinary fewer-braces call is left alone, and members still come from real
// typed nodes.
func (w *scalaWalk) selfTypeBody(n *tsg.Node) *tsg.Node {
	arg := childByType(n, "colon_argument", w.lang)
	if arg == nil {
		return nil
	}
	body := childByType(arg, "indented_block", w.lang)
	if body == nil {
		return nil
	}
	lo, hi := clampU32(n.StartByte()), clampU32(body.StartByte())
	if lo >= hi || hi > len(w.src) || !strings.Contains(string(w.src[lo:hi]), "=>") {
		return nil
	}
	return body
}

func (w *scalaWalk) isBlock(n *tsg.Node) bool {
	typ := n.Type(w.lang)
	return typ == "block" || typ == "indented_block"
}

// callEdges emits EdgeCalls between the callables defined in this file. The call
// site is certain but the callee is resolved by name alone, so the edge is
// heuristic (0.8), down-weighted when the name is ambiguous in the file.
func (w *scalaWalk) callEdges(root *tsg.Node) {
	w.nameCounts = callableNameCounts(w.nodes)
	seen := map[[2]int64]bool{}
	walkCallSites(root,
		scopeByType(w.lang, w.funcIdx, w.declName, "function_definition"),
		func(n *tsg.Node, cur int64) {
			if n.Type(w.lang) == "call_expression" {
				w.maybeCallEdge(n, cur, seen)
			}
		})
}

func (w *scalaWalk) maybeCallEdge(call *tsg.Node, cur int64, seen map[[2]int64]bool) {
	if cur < 0 {
		return
	}
	to, ok := w.funcIdx[w.calleeName(call)]
	if !ok || to == cur {
		return
	}
	key := [2]int64{cur, to}
	if seen[key] {
		return
	}
	seen[key] = true
	w.edges = append(w.edges, heuristicCallEdge(cur, to, w.nodes, w.nameCounts))
}

// calleeName reads the called name off a call expression, following a field
// access to its last segment and a curried call to the identifier starting it.
func (w *scalaWalk) calleeName(call *tsg.Node) string {
	callee := firstNamedChild(call)
	for i := 0; callee != nil && i < 8; i++ {
		switch callee.Type(w.lang) {
		case "identifier":
			return callee.Text(w.src)
		case "field_expression":
			name := ""
			for _, c := range callee.Children() {
				if c.Type(w.lang) == "identifier" {
					name = c.Text(w.src)
				}
			}
			return name
		case "call_expression", "generic_function":
			callee = firstNamedChild(callee)
		default:
			return ""
		}
	}
	return ""
}

func joinScala(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// isScalaComment names the comment nodes. Scaladoc is written `/** … */`, which
// this grammar reports as a plain block_comment, not a doc-specific node, so
// that type must be accepted or every Scaladoc span is lost.
func isScalaComment(typ string) bool {
	return typ == "comment" || typ == "block_comment" || typ == "line_comment"
}
