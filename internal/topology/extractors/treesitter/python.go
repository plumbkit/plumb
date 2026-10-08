// Package treesitter provides gotreesitter-backed topology extractors. It is
// pure Go (no CGo). Compared with the legacy regex extractors it tracks real
// class/function nesting, records accurate end lines, and emits certain
// (confidence 1.0) containment edges; intra-file call edges remain name-resolved
// heuristics (confidence 0.8) because tree-sitter is syntactic, not semantic.
package treesitter

import (
	"context"
	"strings"
	"unicode"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// PythonExtractor extracts Python symbols using the gotreesitter Python grammar.
//
// Concurrency: stateless after construction and safe for concurrent use; each
// Extract call borrows a parser from the shared per-grammar pool and returns it,
// because gotreesitter parsers are not safe for concurrent reuse.
type PythonExtractor struct {
	lang lazyGrammar
}

// NewPython returns a tree-sitter-backed Python extractor.
func NewPython() *PythonExtractor {
	return &PythonExtractor{lang: lazyGrammar{load: grammars.PythonLanguage}}
}

func (e *PythonExtractor) Language() string     { return "python" }
func (e *PythonExtractor) Extensions() []string { return []string{".py"} }

// Extract parses src and returns Python classes, functions, methods, tests,
// imports, and module- and class-level bindings (ALL_CAPS → constant, else
// variable), plus class→member containment edges and intra-file call edges.
// Function-local bindings are not surfaced. Returns (nil, nil, nil) when the
// source cannot be parsed.
func (e *PythonExtractor) Extract(ctx context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	nodes, edges, _, err := e.ExtractWithCallSites(ctx, relPath, src)
	return nodes, edges, err
}

// ExtractWithCallSites is Extract plus the file's decorator sites, from the same
// parse. Only decorators are recorded: they are where a Flask or FastAPI route is
// bound to its handler, and the handler is the decorated declaration itself, so
// no cross-file resolution is needed to name it. Ordinary Python call sites are
// NOT recorded — Python is not a call-graph language (callgraph.go), and a
// partial set of call rows would read as a complete one.
func (e *PythonExtractor) ExtractWithCallSites(ctx context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, []topology.CallSite, error) {
	lang := e.lang.get()
	var sites []topology.CallSite
	nodes, edges, err := extractWith(ctx, lang, src, func(root *tsg.Node) ([]topology.Node, []topology.Edge) {
		w := &pyWalk{lang: lang, src: src, path: relPath, funcIdx: map[string]int64{}}
		w.walk(root, -1, false)
		w.callEdges(root)
		sites = w.sites
		return w.nodes, w.edges
	})
	return nodes, edges, sites, err
}

type pyWalk struct {
	lang       *tsg.Language
	src        []byte
	path       string
	nodes      []topology.Node
	edges      []topology.Edge
	funcIdx    map[string]int64 // function/method/test name → node index, for call edges
	nameCounts map[string]int   // callable Name → count, for ambiguous-call down-weight (#30)
	sites      []topology.CallSite
}

func line(p tsg.Point) int { return int(p.Row) + 1 }

func (w *pyWalk) fieldName(n *tsg.Node) string {
	if nm := n.ChildByFieldName("name", w.lang); nm != nil {
		return nm.Text(w.src)
	}
	return ""
}

func (w *pyWalk) walk(n *tsg.Node, enclosingClass int64, inFunc bool) {
	switch n.Type(w.lang) {
	case "class_definition":
		idx := w.addClass(n)
		w.walkChildren(n, idx, inFunc)
	case "function_definition":
		w.addFunc(n, enclosingClass)
		// Definitions and bindings nested in a function are locals, not
		// methods/attributes of enclosingClass.
		w.walkChildren(n, -1, true)
	case "import_statement", "import_from_statement":
		w.addImports(n)
	case "decorated_definition":
		w.walkDecorated(n, enclosingClass, inFunc)
	case "assignment":
		if !inFunc {
			w.maybeAssignment(n, enclosingClass)
		}
	default:
		w.walkChildren(n, enclosingClass, inFunc)
	}
}

// walkDecorated walks a decorated declaration and records each of its decorators
// as a CallSiteDecorator whose enclosing node is the DECORATED declaration. The
// declaration's node is the first one its walk appends, so its index is known
// before the walk runs; a definition that appends nothing (a nameless one) leaves
// the decorators unattributed rather than pinned to an unrelated node. A def
// nested in a function is still a node, which matters: Flask's application
// factory registers every route inside `create_app()`.
func (w *pyWalk) walkDecorated(n *tsg.Node, enclosingClass int64, inFunc bool) {
	def := n.ChildByFieldName("definition", w.lang)
	if def == nil {
		w.walkChildren(n, enclosingClass, inFunc)
		return
	}
	idx := len(w.nodes)
	w.walk(def, enclosingClass, inFunc)
	if len(w.nodes) == idx {
		return
	}
	for _, c := range n.Children() {
		if c.Type(w.lang) == "decorator" {
			w.addDecorator(c, idx)
		}
	}
}

// addDecorator records one `@expr` decorator. A call decorator
// (`@app.route("/x")`) carries its arguments; a bare one (`@staticmethod`) is
// recorded with none. A decorator whose callee is not a plain name chain
// (`@handlers[0]`) has nothing a consumer could match, and is skipped.
func (w *pyWalk) addDecorator(dec *tsg.Node, enclosing int) {
	expr := firstNamedChild(dec)
	if expr == nil {
		return
	}
	fn, args := expr, (*tsg.Node)(nil)
	if expr.Type(w.lang) == "call" {
		fn = expr.ChildByFieldName("function", w.lang)
		args = expr.ChildByFieldName("arguments", w.lang)
	}
	callee, qualifier := w.dottedParts(fn)
	if callee == "" {
		return
	}
	site := topology.CallSite{
		EnclosingIdx: enclosing,
		Kind:         topology.CallSiteDecorator,
		Callee:       callee,
		Qualifier:    qualifier,
		StartByte:    int(dec.StartByte()),
		StartLine:    line(dec.StartPoint()),
	}
	if args != nil {
		for _, a := range args.Children() {
			if !a.IsNamed() || a.Type(w.lang) == "comment" {
				continue
			}
			site.ArgCount++
			if s, ok := w.stringLiteral(a); ok && !site.HasStringArg {
				site.FirstStringArg, site.HasStringArg = s, true
			}
			if _, q := w.dottedParts(a); a.Type(w.lang) == "identifier" || q != "" {
				if len(site.ArgIdents) < topology.MaxCallSiteArgIdents {
					site.ArgIdents = append(site.ArgIdents, a.Text(w.src))
				}
			}
		}
	}
	w.sites = append(w.sites, site)
}

// dottedParts splits an identifier or attribute chain (`app`, `api.router.get`)
// into its final name and the dotted text to its left. Anything else — a call, a
// subscript — yields ("", "").
func (w *pyWalk) dottedParts(n *tsg.Node) (name, qualifier string) {
	if n == nil {
		return "", ""
	}
	switch n.Type(w.lang) {
	case "identifier":
		return n.Text(w.src), ""
	case "attribute":
		obj := n.ChildByFieldName("object", w.lang)
		attr := n.ChildByFieldName("attribute", w.lang)
		if obj == nil || attr == nil {
			return "", ""
		}
		objName, objQual := w.dottedParts(obj)
		if objName == "" {
			return "", ""
		}
		if objQual != "" {
			objName = objQual + "." + objName
		}
		return attr.Text(w.src), objName
	}
	return "", ""
}

// stringLiteral reports a plain string literal's value, as written between its
// quotes. An f-string with an interpolation and an implicitly concatenated string
// are not literals: their value is not in the source as one span.
func (w *pyWalk) stringLiteral(n *tsg.Node) (string, bool) {
	if n.Type(w.lang) != "string" {
		return "", false
	}
	var start, end *tsg.Node
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "interpolation":
			return "", false
		case "string_start":
			start = c
		case "string_end":
			end = c
		}
	}
	if start == nil || end == nil || end.StartByte() < start.EndByte() {
		return "", false
	}
	return string(w.src[start.EndByte():end.StartByte()]), true
}

func (w *pyWalk) walkChildren(n *tsg.Node, enclosingClass int64, inFunc bool) {
	for _, c := range n.Children() {
		w.walk(c, enclosingClass, inFunc)
	}
}

// maybeAssignment records a module- or class-level binding from an `assignment`
// node: KindConstant when the name is ALL_CAPS (Python's constant convention),
// else KindVariable. Only simple identifier targets are recorded (tuple,
// attribute and subscript targets are skipped). A class-level binding gains a
// certain (1.0/extractor) containment edge.
func (w *pyWalk) maybeAssignment(asn *tsg.Node, enclosingClass int64) {
	left := asn.ChildByFieldName("left", w.lang)
	if left == nil || left.Type(w.lang) != "identifier" {
		return
	}
	name := left.Text(w.src)
	kind := topology.KindVariable
	if isConstName(name) {
		kind = topology.KindConstant
	}
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		StartLine: line(asn.StartPoint()),
		EndLine:   line(asn.EndPoint()),
		Language:  "python",
		Path:      w.path,
	}
	setSpan(&node, asn)
	w.nodes = append(w.nodes, node)
	if enclosingClass >= 0 {
		w.edges = append(w.edges, topology.Edge{
			FromID:     enclosingClass,
			ToID:       idx,
			Kind:       topology.EdgeContains,
			Confidence: 1.0,
			Source:     "extractor",
		})
	}
}

// isConstName reports whether a Python name follows the ALL_CAPS constant
// convention (every cased letter is upper-case, with at least one letter).
func isConstName(name string) bool {
	hasLetter := false
	for _, r := range name {
		if unicode.IsLetter(r) {
			hasLetter = true
			if !unicode.IsUpper(r) {
				return false
			}
		}
	}
	return hasLetter
}

func (w *pyWalk) addClass(n *tsg.Node) int64 {
	name := w.fieldName(n)
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      topology.KindClass,
		Name:      name,
		Qualified: name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "python",
		Path:      w.path,
	}
	setSpan(&node, n)
	node.DocStartByte, node.DocEndByte = pyDocSpan(n, w.lang, w.src)
	w.nodes = append(w.nodes, node)
	return idx
}

// pyIsComment reports whether a Python grammar node type is a comment.
func pyIsComment(typ string) bool { return typ == "comment" }

// pyDocSpan returns the doc-comment span of a Python declaration, and is the
// seam the walk uses instead of calling docSpanBefore directly.
//
// Python puts a declaration's doc comment out of reach of its own previous
// siblings in two ways, and the two compound. A comment preceding the FIRST
// statement of a suite is hoisted OUT of the `block` and parsed as a sibling of
// the block, leaving the declaration as the block's first child with no
// previous sibling at all — so whether a method carried a doc span depended on
// whether something else happened to precede it in the class body. And a
// decorated declaration is a CHILD of its decorated_definition, so its own
// previous sibling is the `@decorator`, exactly as an exported ES declaration
// sits under its export_statement. `@property` on the first method of a class
// hits both at once, which is why the scan climbs in a loop rather than once.
//
// Note what is NOT collected: a Python docstring is the first statement INSIDE
// the declaration, already within its byte span. The span here must precede the
// declaration — move_symbol's include_doc_comment starts its edit range at it —
// so a docstring is out of contract, not merely unimplemented.
func pyDocSpan(decl *tsg.Node, lang *tsg.Language, src []byte) (start, end int) {
	for n := decl; n != nil; n = pyDocAnchor(n, lang) {
		if start, end = docSpanBefore(n, lang, src, pyIsComment); end > start {
			return start, end
		}
	}
	return 0, 0
}

// pyDocAnchor returns the enclosing node whose previous siblings a doc comment
// for n could occupy, or nil when n is already anchored where the comment would
// be. Climbing out of a block is gated on n being its FIRST child: a later
// statement's doc comment is its own previous sibling, and the comment above
// the block belongs to the statement that opens it.
func pyDocAnchor(n *tsg.Node, lang *tsg.Language) *tsg.Node {
	p := n.Parent()
	if p == nil {
		return nil
	}
	switch p.Type(lang) {
	case "decorated_definition":
		return p
	case "block":
		if n.PrevSibling() == nil {
			return p
		}
	}
	return nil
}

func (w *pyWalk) addFunc(n *tsg.Node, enclosingClass int64) {
	name := w.fieldName(n)
	if name == "" {
		return
	}
	kind := topology.KindFunction
	if enclosingClass >= 0 {
		kind = topology.KindMethod
	}
	if isTestName(name) {
		kind = topology.KindTest
	}
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "python",
		Path:      w.path,
	}
	setSpan(&node, n)
	node.DocStartByte, node.DocEndByte = pyDocSpan(n, w.lang, w.src)
	w.nodes = append(w.nodes, node)
	w.funcIdx[name] = idx
	if enclosingClass >= 0 {
		w.edges = append(w.edges, topology.Edge{
			FromID:     enclosingClass,
			ToID:       idx,
			Kind:       topology.EdgeContains,
			Confidence: 1.0,
			Source:     "extractor",
		})
	}
}

func (w *pyWalk) addImports(n *tsg.Node) {
	switch n.Type(w.lang) {
	case "import_from_statement":
		if m := n.ChildByFieldName("module_name", w.lang); m != nil {
			w.addImport(m.Text(w.src), n)
		}
	case "import_statement":
		for _, c := range n.Children() {
			switch c.Type(w.lang) {
			case "dotted_name":
				w.addImport(c.Text(w.src), n)
			case "aliased_import":
				if nm := c.ChildByFieldName("name", w.lang); nm != nil {
					w.addImport(nm.Text(w.src), n)
				}
			}
		}
	}
}

func (w *pyWalk) addImport(name string, n *tsg.Node) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	node := topology.Node{
		Kind:      topology.KindImport,
		Name:      name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "python",
		Path:      w.path,
	}
	setSpan(&node, n)
	w.nodes = append(w.nodes, node)
}

// callEdges does a second pass emitting EdgeCalls between functions defined in
// the same file. The call node is syntactically certain but the callee is
// resolved by name within the file, so confidence is 0.8 (heuristic).
func (w *pyWalk) callEdges(root *tsg.Node) {
	seen := map[[2]int64]bool{}
	w.nameCounts = callableNameCounts(w.nodes)
	walkCallSites(root,
		scopeByType(w.lang, w.funcIdx, w.fieldName, "function_definition"),
		func(n *tsg.Node, curFunc int64) {
			if n.Type(w.lang) == "call" {
				w.maybeCallEdge(n, curFunc, seen)
			}
		})
}

func (w *pyWalk) maybeCallEdge(call *tsg.Node, curFunc int64, seen map[[2]int64]bool) {
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
	w.edges = append(w.edges, heuristicCallEdge(curFunc, to, w.nodes, w.nameCounts))
}

func (w *pyWalk) calleeName(fn *tsg.Node) string {
	switch fn.Type(w.lang) {
	case "identifier":
		return fn.Text(w.src)
	case "attribute":
		if a := fn.ChildByFieldName("attribute", w.lang); a != nil {
			return a.Text(w.src)
		}
	}
	return ""
}

func isTestName(name string) bool {
	return strings.HasPrefix(name, "test_") || strings.HasPrefix(name, "Test")
}
