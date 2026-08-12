package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// DartExtractor extracts Dart symbols using the gotreesitter Dart grammar.
//
// Concurrency: stateless after construction and safe for concurrent use; a
// fresh parser is created per Extract call because gotreesitter parsers are not
// safe for concurrent reuse.
type DartExtractor struct {
	lang lazyGrammar
}

// NewDart returns a tree-sitter-backed Dart extractor.
func NewDart() *DartExtractor {
	return &DartExtractor{lang: lazyGrammar{load: grammars.DartLanguage}}
}

func (e *DartExtractor) Language() string     { return "dart" }
func (e *DartExtractor) Extensions() []string { return []string{".dart"} }

// Extract parses src and returns Dart's declarations: classes, mixins, enums
// and extensions with their members, top-level functions, type aliases,
// top-level variables, `import`/`export`/`part` as imports, and the cases a
// package:test suite declares.
//
// One shape of this grammar drives most of the code below. A function's
// signature and its body are SIBLINGS, not parent and child — `int add(a, b)`
// and `=> a + b` arrive as a `function_signature` followed by a `function_body`
// under the same parent. So every emission has to pair a signature with the body
// that follows it to get a span covering the whole declaration; taking the
// signature's own span would truncate every function in the file at its opening
// brace, which is exactly the kind of span defect the shared guards exist to
// catch.
//
// Locals are deliberately NOT emitted. Dart marks them clearly —
// `local_variable_declaration` and `local_function_declaration` — and a closure
// bound inside a build method is an implementation detail, not a landmark.
func (e *DartExtractor) Extract(ctx context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	lang := e.lang.get()
	return extractWith(ctx, lang, src, func(root *tsg.Node) ([]topology.Node, []topology.Edge) {
		if root == nil {
			return nil, nil
		}
		w := &dartWalk{lang: lang, src: src, path: relPath, funcIdx: map[string]int64{}}
		w.walkTop(root, -1, "")
		w.testCases()
		w.callEdges(root)
		return w.nodes, w.edges
	})
}

type dartWalk struct {
	lang       *tsg.Language
	src        []byte
	path       string
	nodes      []topology.Node
	edges      []topology.Edge
	funcIdx    map[string]int64
	nameCounts map[string]int
	// bodies pairs each callable's body with its node index. The shared
	// walkCallSites/scopeByType pair cannot be used here: it decides which
	// function a call sits inside by descending the signature's subtree, and in
	// this grammar the body is the signature's SIBLING — so every body is
	// outside its own function's scope and no call would ever be attributed.
	bodies []dartBody
}

type dartBody struct {
	node  *tsg.Node
	owner int64
}

// walkTop handles a container's children — the file itself, or a class body —
// pairing each signature with the body that follows it.
func (w *dartWalk) walkTop(n *tsg.Node, parent int64, prefix string) {
	kids := n.Children()
	for i := 0; i < len(kids); i++ {
		c := kids[i]
		switch c.Type(w.lang) {
		case "import_or_export", "part_directive":
			w.addImport(c)
		case "class_definition":
			w.addContainer(c, parent, prefix, "class_body", topology.KindClass)
		case "mixin_declaration":
			// A mixin carries implementations, not just a contract, so it is a
			// class rather than the KindType used for pure interfaces.
			w.addContainer(c, parent, prefix, "class_body", topology.KindClass)
		case "extension_declaration":
			w.addContainer(c, parent, prefix, "extension_body", topology.KindClass)
		case "enum_declaration":
			w.addEnum(c, parent, prefix)
		case "type_alias":
			w.addAlias(c, parent, prefix)
		case "function_signature", "method_signature":
			// The body is the next sibling; consume it so the span covers the
			// whole declaration.
			body := nextBody(kids, i, w.lang)
			w.addCallable(c, body, parent, prefix)
			if body != nil {
				i++
			}
		case "constructor_signature", "factory_constructor_signature", "constant_constructor_signature":
			body := nextBody(kids, i, w.lang)
			w.addConstructor(c, body, parent, prefix)
			if body != nil {
				i++
			}
		case "declaration":
			w.addDeclaration(c, parent, prefix)
		case "static_final_declaration_list", "initialized_identifier_list":
			// At file scope the `const`/`final` keyword is a SIBLING of the
			// declaration list rather than part of it, so the modifier has to
			// come from the preceding node — reading the list's own text would
			// call every top-level binding mutable.
			w.addVariables(c, parent, prefix, dartModifierBefore(kids, i, w.lang))
		case "ERROR":
			// Keep walking through a recovery node: the declarations inside one
			// are still parsed correctly as their own typed nodes, so descending
			// collects them without inventing anything.
			w.walkTop(c, parent, prefix)
		}
	}
}

// addContainer emits a class, mixin or extension and walks its body.
func (w *dartWalk) addContainer(n *tsg.Node, parent int64, prefix, bodyType string, kind topology.NodeKind) {
	name := w.nameOf(n)
	if name == "" {
		return
	}
	idx := w.emit(n, n, parent, prefix, kind, name, w.headOf(n, bodyType))
	if body := childByType(n, bodyType, w.lang); body != nil {
		w.walkTop(body, idx, w.nodes[idx].Qualified)
	}
}

// addEnum emits an enum and its constants. A Dart enum can also declare fields
// and constructors, so its body is walked like any other container.
func (w *dartWalk) addEnum(n *tsg.Node, parent int64, prefix string) {
	name := w.nameOf(n)
	if name == "" {
		return
	}
	idx := w.emit(n, n, parent, prefix, topology.KindClass, name, w.headOf(n, "enum_body"))
	body := childByType(n, "enum_body", w.lang)
	if body == nil {
		return
	}
	qualified := w.nodes[idx].Qualified
	for _, c := range body.Children() {
		if c.Type(w.lang) != "enum_constant" {
			continue
		}
		if id := firstNamedChild(c); id != nil {
			w.emit(c, c, idx, qualified, topology.KindConstant, id.Text(w.src), "")
		}
	}
	w.walkTop(body, idx, qualified)
}

func (w *dartWalk) addAlias(n *tsg.Node, parent int64, prefix string) {
	if name := w.nameOf(n); name != "" {
		w.emit(n, n, parent, prefix, topology.KindType, name, normaliseSpace(n.Text(w.src)))
	}
}

// addCallable emits a function, method, getter or setter. The inner signature
// node carries the name; the outer one is what the class body wraps it in.
func (w *dartWalk) addCallable(sig, body *tsg.Node, parent int64, prefix string) {
	inner := sig
	if sig.Type(w.lang) == "method_signature" {
		if c := firstNamedChild(sig); c != nil {
			inner = c
		}
	}
	switch inner.Type(w.lang) {
	case "constructor_signature", "factory_constructor_signature", "constant_constructor_signature":
		w.addConstructor(inner, body, parent, prefix)
		return
	}
	name := w.callableName(inner)
	if name == "" {
		return
	}
	kind := topology.KindFunction
	if parent >= 0 {
		kind = topology.KindMethod
	}
	if isDartTestName(name) {
		kind = topology.KindTest
	}
	idx := w.emit(sig, body, parent, prefix, kind, name, normaliseSpace(inner.Text(w.src)))
	if body != nil {
		w.bodies = append(w.bodies, dartBody{node: body, owner: idx})
	}
}

// addConstructor emits a constructor, named or factory. Dart names a default
// constructor after its class and a named one `Class.name`, which is how both
// are written at the call site — so the name already carries the class and the
// qualified name must not repeat it (`Counter.zero`, never
// `Counter.Counter.zero`).
func (w *dartWalk) addConstructor(sig, body *tsg.Node, parent int64, prefix string) {
	var parts []string
	for _, c := range sig.Children() {
		if c.Type(w.lang) == "identifier" {
			parts = append(parts, c.Text(w.src))
		}
	}
	if len(parts) == 0 {
		return
	}
	name := strings.Join(parts, ".")
	ownerPrefix := prefix
	if prefix != "" && (name == prefix || strings.HasPrefix(name, prefix+".")) {
		ownerPrefix = ""
	}
	w.emit(sig, body, parent, ownerPrefix, topology.KindMethod, name, normaliseSpace(sig.Text(w.src)))
}

// addDeclaration handles a class-body `declaration`, which wraps fields,
// abstract method signatures and constructors alike.
func (w *dartWalk) addDeclaration(n *tsg.Node, parent int64, prefix string) {
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "function_signature", "getter_signature", "setter_signature":
			// An abstract member: a signature with no body of its own.
			w.addCallable(c, nil, parent, prefix)
			return
		case "constructor_signature", "factory_constructor_signature", "constant_constructor_signature":
			w.addConstructor(c, nil, parent, prefix)
			return
		}
	}
	for _, c := range n.Children() {
		if c.Type(w.lang) == "initialized_identifier_list" || c.Type(w.lang) == "static_final_declaration_list" {
			w.addFields(n, c, parent, prefix)
		}
	}
}

// dartBindingKind decides a binding's kind from its MUTABILITY, not from where
// it sits.
//
// NOT KindField, and not "field because it has a parent": that kind is for a key
// of a data-format file, and topology.KindField's own doc comment says a member
// of a *code* type is KindConstant when immutable and KindVariable otherwise —
// which Java, Kotlin, Swift, Rust and Zig all follow and
// TestExtractors_MemberConventions enforces. Dart marks the distinction plainly,
// so there is no excuse for keying off position: `final` and `const` are the
// immutable case at any scope, everything else is mutable.
func dartBindingKind(decl string) topology.NodeKind {
	for _, f := range strings.Fields(decl) {
		switch f {
		case "final", "const":
			return topology.KindConstant
		case "=", "var":
			// Reached the initialiser or an explicit `var`; no immutable marker
			// preceded it.
			return topology.KindVariable
		}
	}
	return topology.KindVariable
}

// addFields emits the names a field declaration binds, using the declaration
// itself for the span so the type stays inside it.
func (w *dartWalk) addFields(decl, list *tsg.Node, parent int64, prefix string) {
	for _, c := range list.Children() {
		id := firstNamedChild(c)
		if id == nil {
			continue
		}
		name := id.Text(w.src)
		if name == "" {
			continue
		}
		w.emit(decl, decl, parent, prefix, dartBindingKind(decl.Text(w.src)), name, normaliseSpace(decl.Text(w.src)))
	}
}

// addVariables handles top-level `const`/`final`/`var` bindings, which arrive
// as a bare list rather than wrapped in a declaration.
func (w *dartWalk) addVariables(list *tsg.Node, parent int64, prefix string, immutable bool) {
	for _, c := range list.Children() {
		id := firstNamedChild(c)
		if id == nil {
			continue
		}
		name := id.Text(w.src)
		if name == "" {
			continue
		}
		kind := topology.KindVariable
		if immutable {
			kind = topology.KindConstant
		}
		w.emit(c, c, parent, prefix, kind, name, normaliseSpace(c.Text(w.src)))
	}
}

// dartModifierBefore reports whether the node at index i is preceded by a
// `const` or `final` marker, which at file scope is where Dart puts it: the
// keyword is the declaration list's immediately preceding sibling, and anything
// else there (`inferred_type` for `var`, or another declaration) means mutable.
func dartModifierBefore(kids []*tsg.Node, i int, lang *tsg.Language) bool {
	if i == 0 {
		return false
	}
	switch kids[i-1].Type(lang) {
	case "const_builtin", "final_builtin":
		return true
	}
	return false
}

// addImport records `import`, `export` and `part`. An export is a re-export and
// a part is a continuation of the same library; both are dependencies of the
// file in the only sense the Map cares about.
func (w *dartWalk) addImport(n *tsg.Node) {
	uri := w.uriOf(n)
	if uri == "" {
		return
	}
	node := topology.Node{
		Kind:      topology.KindImport,
		Name:      uri,
		Qualified: uri,
		Signature: normaliseSpace(n.Text(w.src)),
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "dart",
		Path:      w.path,
	}
	setSpan(&node, n)
	w.nodes = append(w.nodes, node)
}

// uriOf digs the quoted URI out of an import, export or part directive.
func (w *dartWalk) uriOf(n *tsg.Node) string {
	var found string
	var walk func(*tsg.Node)
	walk = func(c *tsg.Node) {
		if found != "" {
			return
		}
		if c.Type(w.lang) == "string_literal" {
			found = strings.Trim(strings.TrimSpace(c.Text(w.src)), `'"`)
			return
		}
		for _, k := range c.Children() {
			walk(k)
		}
	}
	walk(n)
	return found
}

// emit appends a node spanning from start through end, which for a function is
// its signature through its body.
func (w *dartWalk) emit(start, end *tsg.Node, parent int64, prefix string, kind topology.NodeKind, name, sig string) int64 {
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: joinDart(prefix, name),
		Signature: sig,
		StartLine: line(start.StartPoint()),
		Language:  "dart",
		Path:      w.path,
	}
	setSpan(&node, start)
	if end != nil && end != start {
		node.EndLine = line(end.EndPoint())
		_, endByte, _, endCol := span(end)
		node.EndByte, node.EndCol = endByte, endCol
	} else {
		node.EndLine = line(start.EndPoint())
	}
	node.DocStartByte, node.DocEndByte = docSpanBefore(start, w.lang, w.src, isDartComment)
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
	if _, seen := w.funcIdx[name]; !seen {
		switch kind {
		case topology.KindFunction, topology.KindMethod:
			w.funcIdx[name] = idx
		}
	}
	return idx
}

// callableName reads the name off a function, getter or setter signature.
func (w *dartWalk) callableName(sig *tsg.Node) string {
	for _, c := range sig.Children() {
		if c.Type(w.lang) == "identifier" {
			return c.Text(w.src)
		}
	}
	return ""
}

// nameOf returns a container's declared name, skipping the type arguments and
// supertype references that also appear as identifiers.
func (w *dartWalk) nameOf(n *tsg.Node) string {
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "identifier", "type_identifier":
			return c.Text(w.src)
		}
	}
	return ""
}

// headOf returns a declaration's text up to its body, so a class signature
// keeps its type parameters, `extends` and `implements` without the members.
func (w *dartWalk) headOf(n *tsg.Node, bodyType string) string {
	end := n.EndByte()
	if body := childByType(n, bodyType, w.lang); body != nil {
		end = body.StartByte()
	}
	lo, hi := clampU32(n.StartByte()), clampU32(end)
	if lo >= hi || hi > len(w.src) {
		return ""
	}
	return normaliseSpace(string(w.src[lo:hi]))
}

// testCases emits the cases a package:test suite declares.
//
// Dart tests are not declarations — `test('adds', () { … })` is an ordinary call
// taking a closure, so nothing in walkTop sees them. They are still the symbols
// someone navigates a test file by, and there are a lot of them: over 250 real
// files they accounted for more missing names than every other shape combined.
func (w *dartWalk) testCases() {
	for _, b := range w.bodies {
		w.testsIn(b.node, b.owner, w.nodes[b.owner].Qualified)
	}
}

// testsIn descends one body, nesting a case under the group that encloses it.
func (w *dartWalk) testsIn(n *tsg.Node, parent int64, prefix string) {
	kids := n.Children()
	for i, c := range kids {
		next, nextPrefix := parent, prefix
		if c.Type(w.lang) == "identifier" && i+1 < len(kids) && isDartCallSelector(kids[i+1], w.lang) {
			if kind, ok := dartTestCall(c.Text(w.src)); ok {
				if name := w.firstStringArg(kids[i+1]); name != "" {
					idx := w.emit(c, kids[i+1], parent, prefix, kind, name, c.Text(w.src)+"("+name+")")
					next, nextPrefix = idx, w.nodes[idx].Qualified
				}
			}
		}
		w.testsIn(c, next, nextPrefix)
	}
}

// firstStringArg returns the description a test or group was given.
func (w *dartWalk) firstStringArg(n *tsg.Node) string {
	var found string
	var walk func(*tsg.Node)
	walk = func(c *tsg.Node) {
		if found != "" {
			return
		}
		if c.Type(w.lang) == "string_literal" {
			found = strings.TrimSpace(strings.Trim(strings.TrimSpace(c.Text(w.src)), `'"`))
			return
		}
		for _, k := range c.Children() {
			walk(k)
		}
	}
	walk(n)
	return found
}

// dartTestCall recognises the package:test and flutter_test vocabulary. `group`
// organises, so it becomes a section the cases inside it hang off.
func dartTestCall(callee string) (topology.NodeKind, bool) {
	switch callee {
	case "group":
		return topology.KindSection, true
	case "test", "testWidgets":
		return topology.KindTest, true
	}
	return "", false
}

// callEdges links a call site to the function it names, walking the bodies
// collected during extraction rather than re-descending the tree — see the
// bodies field for why the shared scope helper cannot do this.
func (w *dartWalk) callEdges(_ *tsg.Node) {
	w.nameCounts = callableNameCounts(w.nodes)
	seen := map[[2]int64]bool{}
	for _, b := range w.bodies {
		w.callsIn(b.node, b.owner, seen)
	}
}

// callsIn attributes every call in one body to the function that owns it. An
// identifier is only treated as a call when it is followed by an argument list,
// which is what keeps a bare variable reference from inventing an edge.
func (w *dartWalk) callsIn(n *tsg.Node, owner int64, seen map[[2]int64]bool) {
	kids := n.Children()
	for i, c := range kids {
		if c.Type(w.lang) == "identifier" && i+1 < len(kids) && isDartCallSelector(kids[i+1], w.lang) {
			if target, ok := w.funcIdx[c.Text(w.src)]; ok && target != owner {
				key := [2]int64{owner, target}
				if !seen[key] {
					seen[key] = true
					w.edges = append(w.edges, heuristicCallEdge(owner, target, w.nodes, w.nameCounts))
				}
			}
		}
		w.callsIn(c, owner, seen)
	}
}

// isDartCallSelector reports whether a node is the argument list that turns the
// identifier before it into a call.
func isDartCallSelector(n *tsg.Node, lang *tsg.Language) bool {
	if n.Type(lang) == "arguments" || n.Type(lang) == "argument_part" {
		return true
	}
	if n.Type(lang) != "selector" {
		return false
	}
	for _, c := range n.Children() {
		if c.Type(lang) == "argument_part" {
			return true
		}
	}
	return false
}

// nextBody returns the function_body that follows a signature, or nil when the
// declaration is abstract. The grammar makes them siblings, so this pairing is
// what gives a function a span covering more than its header.
func nextBody(kids []*tsg.Node, i int, lang *tsg.Language) *tsg.Node {
	if i+1 >= len(kids) {
		return nil
	}
	if next := kids[i+1]; next.Type(lang) == "function_body" {
		return next
	}
	return nil
}

// isDartTestName covers the package:test convention of naming a test function
// rather than passing a closure to a runner.
func isDartTestName(name string) bool {
	return strings.HasPrefix(name, "test") && len(name) > 4 && name[4] >= 'A' && name[4] <= 'Z'
}

func joinDart(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// isDartComment names both comment nodes. Dart spells its doc comments `///`,
// which the grammar reports as documentation_comment rather than comment —
// missing that would cost every doc span in an idiomatic package.
func isDartComment(typ string) bool {
	return typ == "comment" || typ == "documentation_comment"
}
