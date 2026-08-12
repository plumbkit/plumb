package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// ObjCExtractor extracts Objective-C symbols using the gotreesitter grammar.
//
// Concurrency: stateless after construction and safe for concurrent use; each
// Extract call borrows a parser from the shared per-grammar pool and returns it,
// because gotreesitter parsers are not safe for concurrent reuse.
type ObjCExtractor struct {
	lang lazyGrammar
}

// NewObjC returns a tree-sitter-backed Objective-C extractor.
func NewObjC() *ObjCExtractor {
	return &ObjCExtractor{lang: lazyGrammar{load: grammars.ObjcLanguage}}
}

func (e *ObjCExtractor) Language() string { return "objc" }

// Extensions covers implementation files only: a `.h` is claimed by the C extractor,
// since both languages use it and nothing inside says which.
func (e *ObjCExtractor) Extensions() []string { return []string{".m", ".mm"} }

// Extract returns Objective-C's declarations: `@interface`, `@implementation` and
// their category/class-extension forms as classes, `@protocol` as a type (a
// contract, mirroring Java's interface), methods keyed by full selector,
// `@property`/ivars/`@synthesize` as constants when readonly and variables
// otherwise, and `#import`/`@import`/`@class` as imports — with certain (1.0)
// containment and heuristic call edges for C calls and message sends. Everything C
// is inherited from cWalk; a `test…` selector in an XCTestCase subclass becomes
// KindTest. Returns (nil, nil, nil) when src cannot be parsed. NOT emitted: locals
// (never descending into a method body suppresses them), block literals bound to a
// local, `@required`/`@optional` (markers, not symbols).
func (e *ObjCExtractor) Extract(ctx context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	lang := e.lang.get()
	return extractWith(ctx, lang, src, func(root *tsg.Node) ([]topology.Node, []topology.Edge) {
		w := &objcWalk{
			cWalk: cWalk{
				lang: lang, src: src, path: relPath, langName: "objc",
				funcIdx: map[string]int64{}, defined: map[string]bool{},
			},
			testClasses: map[string]bool{},
		}
		for _, n := range root.Children() {
			w.top(n)
		}
		w.flushPrototypes()
		w.callGraph(root)
		return w.nodes, w.edges
	})
}

// objcWalk embeds cWalk to inherit every C construct and re-owns exactly the methods
// that RECURSE. Go has no virtual dispatch, so cWalk.top calls *cWalk's* top on the
// children of a `#ifdef` or `extern "C"` block: handing a container down dispatches
// everything nested inside through the C switch, which has no case for
// `@implementation` — so a class guarded by `#if TARGET_OS_IPHONE` vanishes silently
// rather than failing loudly. Leaf handlers recurse into nothing and are delegated
// unchanged. cWalk.qualify is likewise not reused: it joins with ".".
type objcWalk struct {
	cWalk
	// testClasses records classes whose interface declares XCTestCase ancestry, so
	// the implementation below can mark its test methods. One pass suffices because
	// the @interface must precede the @implementation to compile.
	testClasses map[string]bool
}

// top handles one top-level construct, owning the recursion (see the type doc)
// and delegating the C-only leaves to cLeaf.
func (w *objcWalk) top(n *tsg.Node) {
	switch n.Type(w.lang) {
	case "class_interface":
		w.addInterface(n)
	case "class_implementation":
		w.addImplementation(n)
	case "protocol_declaration":
		w.addProtocol(n)
	case "class_declaration":
		w.addForwardDeclarations(n)
	case "module_import":
		w.addModuleImport(n)
	case "type_definition":
		w.addObjCTypedef(n)
	case "implementation_definition":
		// An orphan. A macro attribute inside the class — `AF_API_AVAILABLE(ios(10),
		// macosx(10.12))` — defeats the `@implementation` header while leaving every
		// member intact as a typed node under the ERROR: one real file yields no
		// class_implementation and 82 sound method_definitions. Emitting them unowned
		// beats losing a whole file's API to a header the grammar could not read.
		if inner := firstNamedChild(n); inner != nil {
			w.implMember(n, inner, -1, "", false)
		}
	case "method_definition", "method_declaration":
		w.addMethod(n, n, -1, "", false)
	case "preproc_if", "preproc_ifdef", "preproc_else", "preproc_elif",
		"linkage_specification", "declaration_list":
		for _, c := range n.Children() {
			w.top(c)
		}
	case "ERROR":
		// Keep walking through a recovery node. Where the grammar mis-parses — an
		// `NS_ENUM` body — the ERROR often swallows the rest of the file, yet the
		// classes inside it are still parsed as their own typed nodes. Descending
		// collects those and invents nothing: a symbol must come from a typed node.
		for _, c := range n.Children() {
			w.top(c)
		}
	default:
		w.cLeaf(n)
	}
}

// cLeaf delegates the constructs Objective-C inherits verbatim from C. Every handler
// here emits without descending into a node that could hold an Objective-C
// construct, which is what makes dispatching through cWalk safe.
func (w *objcWalk) cLeaf(n *tsg.Node) {
	switch n.Type(w.lang) {
	case "preproc_include":
		w.addInclude(n)
	case "preproc_def":
		w.addMacro(n, topology.KindConstant)
	case "preproc_function_def":
		w.addMacro(n, topology.KindFunction)
	case "struct_specifier", "union_specifier", "enum_specifier":
		w.addAggregate(n, "")
	case "function_definition":
		w.addFunction(n)
	case "declaration":
		w.addDeclaration(n)
	}
}

// addInterface emits an `@interface`, which in a `.m` is almost always a category or a
// class extension — the primary interface lives in the header. A category is named
// `Foo(Cat)`, its canonical spelling, keeping it distinct from the class it extends; a
// class extension keeps the bare class name, told apart by its signature and span.
func (w *objcWalk) addInterface(n *tsg.Node) {
	name, super, category, isCategory := w.interfaceParts(n)
	if name == "" {
		return
	}
	if super == "XCTestCase" || strings.HasSuffix(super, "TestCase") || strings.HasSuffix(name, "Tests") {
		w.testClasses[name] = true
	}
	owner := ownerName(name, category, isCategory)
	sig := "@interface " + owner
	switch {
	case super != "":
		sig += " : " + super
	case isCategory && category == "":
		sig = "@interface " + name + " ()"
	}
	if protos := w.protocolList(n); protos != "" {
		sig += " " + protos
	}
	idx := w.addNamed(n, n, topology.KindClass, owner, owner, sig)
	w.members(n, idx, owner, w.testClasses[name])
}

// addImplementation emits an `@implementation` — what a reader of a `.m` is after.
func (w *objcWalk) addImplementation(n *tsg.Node) {
	name, _, category, isCategory := w.interfaceParts(n)
	if name == "" {
		// The header parsed but its NAME did not — the grammar inserts a MISSING
		// identifier there. The methods below are still real, so they are emitted
		// unowned rather than lost along with the header.
		w.members(n, -1, "", false)
		return
	}
	owner := ownerName(name, category, isCategory)
	idx := w.addNamed(n, n, topology.KindClass, owner, owner, "@implementation "+owner)
	w.members(n, idx, owner, w.testClasses[name] || strings.HasSuffix(name, "Tests"))
}

// addProtocol emits a `@protocol` as KindType rather than KindClass, matching Java's and
// Kotlin's interfaces: a contract with no storage or implementation is a different thing
// from a class, and conflating them makes "which classes are here" useless.
func (w *objcWalk) addProtocol(n *tsg.Node) {
	id := childByType(n, "identifier", w.lang)
	if id == nil {
		return
	}
	name := id.Text(w.src)
	sig := "@protocol " + name
	if protos := w.protocolList(n); protos != "" {
		sig += " " + protos
	}
	idx := w.addNamed(n, n, topology.KindType, name, name, sig)
	w.members(n, idx, name, false)
}

// members walks a container's body one level deep. It never descends into a method
// body, which is what suppresses locals.
func (w *objcWalk) members(container *tsg.Node, parent int64, owner string, testCtx bool) {
	for _, c := range container.Children() {
		switch c.Type(w.lang) {
		case "method_declaration", "method_definition":
			w.addMethod(c, c, parent, owner, testCtx)
		case "implementation_definition":
			// A wrapper holding a single member. The WRAPPER is the doc anchor: a
			// comment above the method is the wrapper's previous sibling, and the
			// method node itself has none.
			if inner := firstNamedChild(c); inner != nil {
				w.implMember(c, inner, parent, owner, testCtx)
			}
		case "property_declaration":
			if decl := childByType(c, "struct_declaration", w.lang); decl != nil {
				w.addField(c, w.structDeclaratorName(decl), parent, owner)
			}
		case "instance_variables":
			w.addInstanceVariables(c, parent, owner)
		case "ERROR":
			// A defect inside the body — a `#if` splitting an expression — must not
			// cost the members after it, which are still typed nodes and still
			// belong to THIS container.
			w.members(c, parent, owner, testCtx)
		case "qualified_protocol_interface_declaration":
			// `@required` / `@optional` group methods under a node of their own.
			// The marker is not a symbol; the methods under it are.
			w.members(c, parent, owner, testCtx)
		}
	}
}

// implMember dispatches the single child of an implementation_definition.
func (w *objcWalk) implMember(wrapper, inner *tsg.Node, parent int64, owner string, testCtx bool) {
	switch inner.Type(w.lang) {
	case "method_definition":
		w.addMethod(inner, wrapper, parent, owner, testCtx)
	case "property_implementation":
		// `@synthesize` / `@dynamic`. When the property is declared in the header
		// this line is its only mention in the file, so dropping it would leave
		// the backing storage with no symbol at all.
		if id := childByType(inner, "identifier", w.lang); id != nil {
			w.addField(inner, id.Text(w.src), parent, owner)
		}
	default:
		w.top(inner)
	}
}

// addMethod emits one method, named by its FULL selector — its real identity.
// `initWithFrame:` and `initWithCoder:` are both `init…` up to the first colon, so
// first-keyword naming merges unrelated symbols; the full selector is also what a
// backtrace, a `@selector()` literal and the docs print. Trailing colons survive
// for the same reason: `setValue:` is not `setValue`.
func (w *objcWalk) addMethod(n, doc *tsg.Node, parent int64, owner string, testCtx bool) {
	selector, classMethod := w.selectorOf(n)
	if selector == "" {
		return
	}
	kind := topology.KindMethod
	if testCtx && strings.HasPrefix(selector, "test") {
		kind = topology.KindTest
	}
	idx := w.addNamed(n, doc, kind, selector, methodQualified(owner, selector, classMethod), w.methodSignature(n))
	w.link(parent, idx)
	w.noteFunc(selector, idx)
}

// addField emits a `@property`, ivar or `@synthesize` — NOT as KindField: that kind
// is for keys of a data-format file, and its doc says a member of a *code* type is
// KindConstant when immutable and KindVariable otherwise, which Java and Kotlin
// follow and TestExtractors_MemberConventions enforces. A `readonly` property
// publishes no setter, so it is the immutable case. The qualified name is dotted:
// for DATA members that is the Objective-C spelling.
func (w *objcWalk) addField(n *tsg.Node, name string, parent int64, owner string) {
	if name == "" {
		return
	}
	decl := normaliseSpace(n.Text(w.src))
	kind := topology.KindVariable
	// Matched against the attribute list alone, so a property merely named
	// `readonlyThing` is not misread as immutable.
	if l, r := strings.IndexByte(decl, '('), strings.IndexByte(decl, ')'); l >= 0 && r > l &&
		strings.Contains(","+strings.ReplaceAll(decl[l+1:r], " ", "")+",", ",readonly,") {
		kind = topology.KindConstant
	}
	idx := w.addNamed(n, n, kind, name, owner+"."+name, decl)
	w.link(parent, idx)
}

// addInstanceVariables emits the ivars of a `{ … }` block. A visibility marker
// (`@private`, `@protected`) arrives as an instance_variable of its own with no
// declaration inside and is skipped: it qualifies the ivars that follow.
func (w *objcWalk) addInstanceVariables(n *tsg.Node, parent int64, owner string) {
	for _, iv := range n.Children() {
		if iv.Type(w.lang) != "instance_variable" {
			continue
		}
		if decl := childByType(iv, "struct_declaration", w.lang); decl != nil {
			w.addField(iv, w.structDeclaratorName(decl), parent, owner)
		}
	}
}

// addForwardDeclarations emits `@class Foo, Bar;` as imports rather than types. The
// construct names classes defined in ANOTHER file; emitting them as types would put a
// definition-shaped node where there is no definition, and an agent following it would
// land on a one-line forward declaration. As an import it says the true thing.
func (w *objcWalk) addForwardDeclarations(n *tsg.Node) {
	for _, c := range n.Children() {
		if c.Type(w.lang) == "identifier" {
			name := c.Text(w.src)
			w.addNamed(c, c, topology.KindImport, name, name, "@class "+name)
		}
	}
}

// addModuleImport emits `@import Foundation;` and its dotted submodule form,
// rejoining the parts so the name matches how the module is written.
func (w *objcWalk) addModuleImport(n *tsg.Node) {
	var parts []string
	for _, c := range n.Children() {
		if c.Type(w.lang) == "identifier" {
			parts = append(parts, c.Text(w.src))
		}
	}
	if len(parts) == 0 {
		return
	}
	name := strings.Join(parts, ".")
	w.addNamed(n, n, topology.KindImport, name, name, "@import "+name)
}

// addObjCTypedef names the two typedef shapes C's handler gets wrong, delegating
// the rest to it. `typedef void (^Block)(…)` buries its name two declarators deep,
// past C's scan for a DIRECT type_identifier, so a block typedef — how every async
// Objective-C API spells its callback — is dropped or mis-named after its return
// type. `typedef NS_ENUM(NSInteger, Name) { … }` is a macro the grammar cannot see
// through: its only direct type_identifier is the MACRO name, so cWalk.addTypedef
// would emit a symbol called NS_ENUM; the real one is the last type_identifier in
// the parenthesised arguments.
func (w *objcWalk) addObjCTypedef(n *tsg.Node) {
	var name string
	head := childByType(n, "type_identifier", w.lang)
	args := childByType(n, "parenthesized_declarator", w.lang)
	if block := w.findBlockDeclarator(n); block != nil {
		if id := childByType(block, "type_identifier", w.lang); id != nil {
			name = id.Text(w.src)
		}
	} else if head != nil && args != nil && strings.HasPrefix(head.Text(w.src), "NS_") {
		for _, c := range args.Children() {
			if c.Type(w.lang) == "type_identifier" {
				name = c.Text(w.src) // the LAST one names the enum
			}
		}
	}
	if name == "" {
		w.addTypedef(n)
		return
	}
	w.addNamed(n, n, topology.KindType, name, name, normaliseSpace(n.Text(w.src)))
}

// findBlockDeclarator returns the block_pointer_declarator holding a block typedef's
// name, or nil. It recurses: the declarator sits under one wrapper per type piece.
func (w *objcWalk) findBlockDeclarator(n *tsg.Node) *tsg.Node {
	for _, c := range n.Children() {
		if c.Type(w.lang) == "block_pointer_declarator" {
			return c
		}
		if d := w.findBlockDeclarator(c); d != nil {
			return d
		}
	}
	return nil
}

// addNamed is the one emission site for every Objective-C node, so a new construct cannot
// forget its byte or doc span. Containment is the caller's business.
func (w *objcWalk) addNamed(n, doc *tsg.Node, kind topology.NodeKind, name, qualified, sig string) int64 {
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
	node.DocStartByte, node.DocEndByte = docSpanBefore(doc, w.lang, w.src, isCComment)
	w.nodes = append(w.nodes, node)
	return idx
}

// interfaceParts pulls a class_interface / class_implementation apart into its
// name, superclass and category name.
// The unnamed punctuation separates the two shapes: `@interface Foo : NSObject` and
// `@interface Foo (Cat)` both parse as two bare identifiers, so only the `:` or `(`
// between them says which is which. A class extension is the parenthesised form
// with no identifier inside, hence the flag rather than a category name.
func (w *objcWalk) interfaceParts(n *tsg.Node) (name, super, category string, isCategory bool) {
	var afterColon, inParens bool
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case ":":
			afterColon = true
		case "(":
			inParens, isCategory = true, true
		case ")":
			inParens = false
		case "identifier":
			switch {
			case name == "":
				name = c.Text(w.src)
			case afterColon && super == "":
				super = c.Text(w.src)
			case inParens && category == "":
				category = c.Text(w.src)
			}
		}
	}
	return name, super, category, isCategory
}

// protocolList returns the adopted-protocol clause as written. The grammar spells it two
// ways — parameterized_arguments after a class, protocol_reference_list after a protocol.
func (w *objcWalk) protocolList(n *tsg.Node) string {
	for _, typ := range []string{"parameterized_arguments", "protocol_reference_list"} {
		if c := childByType(n, typ, w.lang); c != nil {
			return normaliseSpace(c.Text(w.src))
		}
	}
	return ""
}

// selectorOf reconstructs a selector and reports whether the method is a class
// method: `- (void)a:(int)x b:(int)y` yields "a:b:", an argument-less method its bare
// keyword. Keywords are the method's DIRECT identifier children (an argument's name
// nests inside its method_parameter and never reaches this loop); the
// method_parameter count decides whether colons belong.
func (w *objcWalk) selectorOf(n *tsg.Node) (selector string, classMethod bool) {
	var keywords []string
	params := 0
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "+":
			classMethod = true
		case "identifier":
			keywords = append(keywords, c.Text(w.src))
		case "method_parameter":
			params++
		}
	}
	switch {
	case len(keywords) == 0:
		return "", classMethod
	case params == 0:
		return keywords[0], classMethod
	}
	return strings.Join(keywords, ":") + ":", classMethod
}

// messageSelector reconstructs the selector a message send invokes, so
// `[self doThing:x with:y]` resolves to the method named "doThing:with:".
//
// A keyword is told from an argument by the colon that follows it: the grammar
// flattens receiver, keywords and arguments into one child list, where a bare
// identifier argument is indistinguishable from a keyword. A unary message has no
// colon: its selector is the lone identifier after the receiver.
func (w *objcWalk) messageSelector(n *tsg.Node) string {
	kids := n.Children()
	var keywords []string
	for i, c := range kids {
		if c.Type(w.lang) == "identifier" && i+1 < len(kids) && kids[i+1].Type(w.lang) == ":" {
			keywords = append(keywords, c.Text(w.src))
		}
	}
	if len(keywords) > 0 {
		return strings.Join(keywords, ":") + ":"
	}
	var named []*tsg.Node
	for _, c := range kids {
		if c.IsNamed() {
			named = append(named, c)
		}
	}
	if len(named) == 2 && named[1].Type(w.lang) == "identifier" {
		return named[1].Text(w.src)
	}
	return ""
}

// methodSignature reproduces the declaration head — `- (NSString *)joinFirst:…`
// — without the body, which is what an outline wants to show.
func (w *objcWalk) methodSignature(n *tsg.Node) string {
	start, end := clampU32(n.StartByte()), clampU32(n.EndByte())
	if body := childByType(n, "compound_statement", w.lang); body != nil {
		end = clampU32(body.StartByte())
	}
	if end <= start || end > len(w.src) {
		return ""
	}
	return strings.TrimSuffix(normaliseSpace(string(w.src[start:end])), ";")
}

// structDeclaratorName digs the declared name out of a struct_declaration, the node
// both `@property` and an ivar wrap their declaration in. cWalk's declaratorName cannot
// start there — struct_declarator is not a wrapper it descends — but handles the rest.
func (w *objcWalk) structDeclaratorName(decl *tsg.Node) string {
	sd := childByType(decl, "struct_declarator", w.lang)
	if sd == nil {
		return ""
	}
	return w.declaratorName(sd)
}

// callGraph attributes each call site to the method or function whose body contains
// it, resolving both C calls and message sends by name. A send naming no selector
// this file defines is dropped rather than guessed at: the call graph is intra-file
// and most real messages go to a framework class, so guessing would fabricate more
// edges than it found.
func (w *objcWalk) callGraph(root *tsg.Node) {
	w.nameCounts = callableNameCounts(w.nodes)
	seen := map[[2]int64]bool{}
	enclosing := func(n *tsg.Node, cur int64) int64 {
		switch n.Type(w.lang) {
		case "method_definition":
			if sel, _ := w.selectorOf(n); sel != "" {
				if idx, ok := w.funcIdx[sel]; ok {
					return idx
				}
			}
		case "function_definition":
			if decl := w.findDeclarator(n); decl != nil {
				if idx, ok := w.funcIdx[w.declaratorName(decl)]; ok {
					return idx
				}
			}
		}
		return cur
	}
	walkCallSites(root, enclosing, func(n *tsg.Node, cur int64) {
		if cur < 0 {
			return
		}
		callee := w.calleeName(n)
		target, ok := w.funcIdx[callee]
		if callee == "" || !ok || target == cur || seen[[2]int64{cur, target}] {
			return
		}
		seen[[2]int64{cur, target}] = true
		w.edges = append(w.edges, heuristicCallEdge(cur, target, w.nodes, w.nameCounts))
	})
}

// calleeName returns the name a call site invokes, or "" for a node that is not
// one.
func (w *objcWalk) calleeName(n *tsg.Node) string {
	switch n.Type(w.lang) {
	case "call_expression":
		if c := firstNamedChild(n); c != nil {
			return c.Text(w.src)
		}
	case "message_expression":
		return w.messageSelector(n)
	}
	return ""
}

// ownerName: `Foo` for a class, `Foo(Cat)` for a category — the canonical spelling.
func ownerName(name, category string, isCategory bool) string {
	if isCategory && category != "" {
		return name + "(" + category + ")"
	}
	return name
}

// methodQualified spells a method the way Objective-C does:
// `-[NSString stringByAppendingString:]` — what a crash log, a documentation page
// and `__PRETTY_FUNCTION__` all print, so what someone recognises and searches for.
// The dotted form C uses is actively wrong here: dot syntax in Objective-C means
// property access, so a class with both a `name` property and a `- (void)name`
// method would produce two symbols with the one qualified name, and the `-`/`+`
// carries the instance-versus-class distinction free besides. Downstream name_path
// resolution splits on any non-identifier character, so `-[Foo doThing]` resolves
// as Foo/doThing.
func methodQualified(owner, selector string, classMethod bool) string {
	if owner == "" {
		return selector
	}
	sigil := "-"
	if classMethod {
		sigil = "+"
	}
	return sigil + "[" + owner + " " + selector + "]"
}
