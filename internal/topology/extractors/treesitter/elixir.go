package treesitter

import (
	"context"
	"strconv"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// ElixirExtractor extracts Elixir symbols using the gotreesitter Elixir grammar.
//
// Concurrency: stateless after construction and safe for concurrent use; a
// fresh parser is created per Extract call because gotreesitter parsers are not
// safe for concurrent reuse.
type ElixirExtractor struct {
	lang lazyGrammar
}

// NewElixir returns a tree-sitter-backed Elixir extractor.
func NewElixir() *ElixirExtractor {
	return &ElixirExtractor{lang: lazyGrammar{load: grammars.ElixirLanguage}}
}

func (e *ElixirExtractor) Language() string { return "elixir" }

// Extensions covers both source forms: `.exs` is script, but holds every test
// module and `mix.exs` project definition.
func (e *ElixirExtractor) Extensions() []string { return []string{".ex", ".exs"} }

// Extract parses src and returns Elixir's declarations: modules and protocol
// implementations as packages, protocols as types, def/defp/defmacro and kin as
// functions, defstruct fields as variables, @type and @callback as types and
// contracts, alias/import/require/use as imports and ExUnit blocks as tests,
// with certain (1.0) containment edges and heuristic call edges.
//
// Elixir has no declaration keywords: `defmodule`, `def` and `alias` are
// ordinary macro calls, so the grammar offers no typed definition node. They all
// parse to one shape —
//
//	call
//	  identifier      "def"          <- the "keyword" is just the callee
//	  arguments
//	    call           "greet(name)" <- the head: name and parameters
//	  do_block                       <- or a `do:` pair inside arguments
//
// — so this keys off the CALLEE NAME and reads the first named child of
// `arguments` as the head. One shape covers both body spellings (`do … end` and
// `do:` differ only in where the body hangs), guards (`when` wraps the head in a
// binary_operator), operator definitions (`def a <> b`, where the head IS the
// binary_operator) and module attributes (`@type t() :: …` is a unary_operator
// over the same shape). Nothing matches source text.
//
// Two judgment calls. Arity lives in the QUALIFIED name (`handle_call/3`, what a
// stacktrace prints) and never in Name, which funcIdx and every call-edge lookup
// key on; Signature keeps the human head, and is where public-versus-private
// survives. And each CLAUSE is its own node — a merged one needs a span over
// non-contiguous text, and spans are edit ranges for replace_symbol_body.
func (e *ElixirExtractor) Extract(ctx context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	lang := e.lang.get()
	return extractWith(ctx, lang, src, func(root *tsg.Node) ([]topology.Node, []topology.Edge) {
		w := &elixirWalk{
			lang: lang, src: src, path: relPath,
			funcIdx: map[string]int64{}, headers: map[uint32]bool{}, seenImport: map[string]bool{},
		}
		w.walk(root, -1, "")
		w.callEdges(root)
		return w.nodes, w.edges
	})
}

type elixirWalk struct {
	lang       *tsg.Language
	src        []byte
	path       string
	nodes      []topology.Node
	edges      []topology.Edge
	funcIdx    map[string]int64
	nameCounts map[string]int
	seenImport map[string]bool
	// headers holds the start byte of every declaration HEAD call — the
	// `greet(name)` inside `def greet(name)`, syntactically a call, which the
	// call-edge pass would otherwise read as a call to the first clause.
	headers map[uint32]bool
}

// walk descends carrying the enclosing module's node index and its dotted name.
// There is no locals-suppression flag, unlike the Ruby and Lua walks: an Elixir
// local is a pattern match (`total = 5`, a binary_operator) that never resembles
// a declaration, so keying emission off the def* callee names excludes both it
// and the real risk here, an ordinary CALL read as a symbol.
func (w *elixirWalk) walk(n *tsg.Node, parent int64, prefix string) {
	if n == nil {
		return // a declaration with no `do` block: nothing to descend
	}
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "call":
			w.addCall(c, parent, prefix)
		case "unary_operator":
			w.addAttribute(c, parent, prefix)
		default:
			// Everything else is descended through, ERROR recovery nodes and
			// the hidden `…_repeat1` nodes beneath them included: one unclosed
			// `do` wraps a file's remainder in a single ERROR, yet the defs
			// inside still parse as their own typed nodes, so descending
			// recovers them — every symbol still from a real node, never text.
			w.walk(c, parent, prefix)
		}
	}
}

// addCall dispatches on the callee name, all that separates a declaration here.
func (w *elixirWalk) addCall(n *tsg.Node, parent int64, prefix string) {
	kw := w.callTarget(n)
	switch kw {
	case "defmodule":
		w.addScope(n, kw, topology.KindPackage, parent, prefix)
	case "defprotocol":
		w.addScope(n, kw, topology.KindType, parent, prefix)
	case "defimpl":
		w.addImpl(n, parent)
	case "def", "defp", "defmacro", "defmacrop", "defguard", "defguardp", "defdelegate":
		w.addFunction(n, kw, parent, prefix)
	case "defstruct", "defexception":
		w.addStructFields(n, parent, prefix)
	case "alias", "import", "require", "use":
		w.addImports(n)
	case "test", "property", "describe":
		w.addExample(n, kw, parent, prefix)
	default:
		// An ordinary call, whose `do` block may hold declarations anyway:
		// `quote do def … end` in a macro is idiomatic.
		w.walk(n, parent, prefix)
	}
}

// addScope emits `defmodule` and `defprotocol`. A module is a KindPackage — the
// mapping Go's extractor uses for its package clause and php.go for a namespace
// — because nothing instantiates it, its functions are reached as `Mod.fun/2`,
// and it is the unit of namespacing; a type kind would imply a values-of-this-
// type relationship the language lacks. A protocol IS a contract over values, so
// it is a KindType. Name is the LAST segment: `alias` exists precisely so code
// refers to `MyApp.Accounts.User` as `User`.
func (w *elixirWalk) addScope(n *tsg.Node, kw string, kind topology.NodeKind, parent int64, prefix string) {
	body := childByType(n, "do_block", w.lang)
	name := w.argText(n, "alias")
	if name == "" {
		// `defmodule unquote(mod) do` builds its name at compile time, so there
		// is none to record — but its declarations are real, so they take the
		// caller's scope rather than being dropped with the module.
		w.walk(body, parent, prefix)
		return
	}
	qualified := elixirQualify(prefix, name)
	doc, isTest := w.scanBody(body)
	if isTest {
		kind = topology.KindTest
	}
	idx := w.emit(n, parent, kind, elixirLastSegment(name), qualified, kw+" "+qualified)
	w.nodes[idx].Docstring = doc
	w.walk(body, idx, qualified)
}

// addImpl emits `defimpl Size, for: BitString` under the name Elixir compiles it
// to — the module `Size.BitString` — because someone asking how Size is
// implemented for BitString searches the type, not the protocol. `for:` is
// defimpl's only keyword option, so the keyword list's alias is that target; a
// list target supplies no single alias, so the node keeps the protocol's name
// and the head survives in Signature.
func (w *elixirWalk) addImpl(n *tsg.Node, parent int64) {
	proto := w.argText(n, "alias")
	if proto == "" {
		return
	}
	name, qualified := elixirLastSegment(proto), proto
	if kws := w.argChild(n, "keywords"); kws != nil {
		for _, p := range kws.Children() {
			if a := childByType(p, "alias", w.lang); a != nil {
				name, qualified = elixirLastSegment(a.Text(w.src)), proto+"."+a.Text(w.src)
				break
			}
		}
	}
	sig := "defimpl"
	if args := childByType(n, "arguments", w.lang); args != nil {
		sig += " " + elixirFlatten(args.Text(w.src))
	}
	idx := w.emit(n, parent, topology.KindPackage, name, qualified, sig)
	w.walk(childByType(n, "do_block", w.lang), idx, qualified)
}

// addFunction emits one node per def/defp/defmacro/… clause. kw is the defining
// macro, kept verbatim at the front of Signature so private stays visible.
func (w *elixirWalk) addFunction(n *tsg.Node, kw string, parent int64, prefix string) {
	idx := w.addDecl(n, n, kw, topology.KindFunction, parent, prefix)
	if idx < 0 {
		return
	}
	// The FIRST clause wins, so a call site resolves to one node rather than to
	// whichever clause happened to be walked last.
	name := w.nodes[idx].Name
	if _, seen := w.funcIdx[name]; !seen {
		w.funcIdx[name] = idx
	}
	// A macro's `quote do … end` declares real functions; they belong to the
	// macro that generates them.
	w.walk(childByType(n, "do_block", w.lang), idx, prefix)
}

// addDecl emits one name/arity declaration — a def clause or a @type/@callback,
// which share the head shape exactly. span is the node the symbol covers (the
// whole `@…` for an attribute); headCall carries the head.
func (w *elixirWalk) addDecl(span, headCall *tsg.Node, sigPrefix string, kind topology.NodeKind, parent int64, prefix string) int64 {
	name, arity, raw, head := w.headParts(headCall)
	if name == "" {
		return -1
	}
	if head != nil && head.Type(w.lang) == "call" {
		w.headers[head.StartByte()] = true
	}
	sig := sigPrefix
	if raw != nil {
		sig += " " + elixirFlatten(raw.Text(w.src))
	}
	return w.emit(span, parent, kind, name, elixirQualify(prefix, name)+"/"+strconv.Itoa(arity), sig)
}

// addAttribute handles the attributes that DECLARE something: `@type`, `@typep`
// and `@opaque` name a type, `@callback` and `@macrocallback` a contract a
// behaviour requires. `@spec` is deliberately NOT a node — it annotates the def
// below it and carries no name that def lacks, so a node would double every
// function in the Map. Nor is it lost: docSpan folds it into that def's span.
func (w *elixirWalk) addAttribute(n *tsg.Node, parent int64, prefix string) {
	inner := childByType(n, "call", w.lang)
	if inner == nil {
		return
	}
	kw := w.callTarget(inner)
	var kind topology.NodeKind
	switch kw {
	case "type", "typep", "opaque":
		kind = topology.KindType
	case "callback", "macrocallback":
		kind = topology.KindFunction
	default:
		return
	}
	w.addDecl(n, inner, "@"+kw, kind, parent, prefix)
}

// addStructFields emits one node per field of `defstruct`/`defexception`. They
// are KindVariable, not KindField: KindField's own doc comment reserves that for
// keys of a data-format file and sends a member of a *code* type to KindConstant
// or KindVariable. There is no node for the defstruct itself — it IS the module.
func (w *elixirWalk) addStructFields(n *tsg.Node, parent int64, prefix string) {
	// `defstruct [:a, b: 1]` wraps the fields in a list, `defstruct a: 1` does
	// not, and both spellings are ubiquitous.
	container := w.argChild(n, "list")
	if container == nil {
		container = childByType(n, "arguments", w.lang)
	}
	if container == nil {
		return
	}
	for _, c := range container.Children() {
		switch c.Type(w.lang) {
		case "atom":
			w.addStructField(c, c.Text(w.src), parent, prefix)
		case "keywords":
			for _, p := range c.Children() {
				if k := childByType(p, "keyword", w.lang); k != nil {
					w.addStructField(p, k.Text(w.src), parent, prefix)
				}
			}
		}
	}
}

// addStructField emits one field from its `:atom` or `key:` raw text.
func (w *elixirWalk) addStructField(n *tsg.Node, raw string, parent int64, prefix string) {
	if name := strings.Trim(strings.TrimSpace(raw), ":"); name != "" {
		w.emit(n, parent, topology.KindVariable, name, elixirQualify(prefix, name), elixirFlatten(n.Text(w.src)))
	}
}

// addImports emits alias/import/require/use as KindImport: all four bring an
// outside module into scope, and the difference is a compile-time detail no
// dependency question turns on. `alias MyApp.{Repo, User}` expands per alias,
// since collapsing to `MyApp` would erase what is being asked for.
func (w *elixirWalk) addImports(n *tsg.Node) {
	args := childByType(n, "arguments", w.lang)
	if args == nil {
		return
	}
	first := firstNamedChild(args)
	if first == nil {
		return
	}
	switch first.Type(w.lang) {
	case "alias":
		w.addImport(n, first.Text(w.src))
	case "dot":
		base, tuple := firstNamedChild(first), childByType(first, "tuple", w.lang)
		if base == nil || tuple == nil {
			return
		}
		for _, a := range tuple.Children() {
			if a.Type(w.lang) == "alias" {
				w.addImport(a, base.Text(w.src)+"."+a.Text(w.src))
			}
		}
	}
}

// addImport records one dependency, deduplicated by name.
func (w *elixirWalk) addImport(n *tsg.Node, name string) {
	if name == "" || w.seenImport[name] {
		return
	}
	w.seenImport[name] = true
	w.emit(n, -1, topology.KindImport, name, name, "")
}

// addExample emits an ExUnit `test "…"`/`describe "…"` (and StreamData's
// `property "…"`) as a test named by its description, a describe parenting the
// tests inside it so the suite's shape survives. With no description it is not
// an ExUnit macro, so it is walked as ordinary.
func (w *elixirWalk) addExample(n *tsg.Node, kw string, parent int64, prefix string) {
	name := w.stringArg(n)
	if name == "" {
		w.walk(n, parent, prefix)
		return
	}
	idx := w.emit(n, parent, topology.KindTest, name, elixirQualify(prefix, name), kw+" "+strconv.Quote(name))
	w.walk(childByType(n, "do_block", w.lang), idx, prefix)
}

// headParts reads a def-style call's head: the name, arity, the head as written
// (for the signature) and the head with its wrappers stripped.
func (w *elixirWalk) headParts(n *tsg.Node) (name string, arity int, raw, head *tsg.Node) {
	args := childByType(n, "arguments", w.lang)
	if args == nil {
		return "", 0, nil, nil
	}
	raw = firstNamedChild(args)
	head = w.unwrapHead(raw)
	if head == nil {
		return "", 0, raw, nil
	}
	switch head.Type(w.lang) {
	case "identifier":
		return head.Text(w.src), 0, raw, head // `def ping, do: :pong`
	case "call":
		id := childByType(head, "identifier", w.lang)
		if id == nil {
			return "", 0, raw, head // `def unquote(name)(x)` names itself at compile time
		}
		return id.Text(w.src), w.arity(head), raw, head
	case "binary_operator":
		// An operator definition, `def a <> b`: the name is the operator and
		// its two operands are the arguments.
		return w.operatorToken(head), 2, raw, head
	}
	return "", 0, raw, head
}

// unwrapHead strips the wrappers OUTSIDE a declaration head: `when` for a guard,
// `::` for a type or callback spec. Both parse as a binary_operator whose left
// side is the head proper, and both can stack — hence the loop.
func (w *elixirWalk) unwrapHead(n *tsg.Node) *tsg.Node {
	for i := 0; n != nil && i < 4 && n.Type(w.lang) == "binary_operator"; i++ {
		if op := w.operatorToken(n); op != "when" && op != "::" {
			return n
		}
		n = firstNamedChild(n)
	}
	return n
}

// operatorToken returns a binary_operator's operator, its one anonymous child.
func (w *elixirWalk) operatorToken(n *tsg.Node) string {
	for _, c := range n.Children() {
		if !c.IsNamed() {
			return c.Text(w.src)
		}
	}
	return ""
}

// arity counts a head call's parameters. Parentheses are anonymous children of
// the inner `arguments`, so counting NAMED children answers for `f()`, `f(a, b)`
// and paren-less `def f a`.
func (w *elixirWalk) arity(head *tsg.Node) (n int) {
	if args := childByType(head, "arguments", w.lang); args != nil {
		for _, c := range args.Children() {
			if c.IsNamed() {
				n++
			}
		}
	}
	return n
}

// callTarget returns a call's callee: the bare identifier for `def foo`, the
// last segment for a qualified `String.upcase(s)`.
func (w *elixirWalk) callTarget(n *tsg.Node) string {
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "identifier":
			return c.Text(w.src)
		case "dot":
			return elixirLastSegment(elixirFlatten(c.Text(w.src)))
		case "arguments", "do_block":
			return ""
		}
	}
	return ""
}

// argChild returns the first child of n's `arguments` of the given type.
func (w *elixirWalk) argChild(n *tsg.Node, typ string) *tsg.Node {
	if n == nil {
		return nil
	}
	if args := childByType(n, "arguments", w.lang); args != nil {
		return childByType(args, typ, w.lang)
	}
	return nil
}

func (w *elixirWalk) argText(n *tsg.Node, typ string) string {
	if c := w.argChild(n, typ); c != nil {
		return c.Text(w.src)
	}
	return ""
}

// stringArg returns a call's first string argument, quotes stripped.
func (w *elixirWalk) stringArg(n *tsg.Node) string {
	s := w.argChild(n, "string")
	if s == nil {
		return ""
	}
	if c := childByType(s, "quoted_content", w.lang); c != nil {
		return elixirFlatten(c.Text(w.src))
	}
	return strings.Trim(s.Text(w.src), `"`)
}

// scanBody reads the two things a module body says about the module itself: its
// @moduledoc, and whether it is an ExUnit case — a structural marker, stronger
// than the `_test.exs` filename convention. The @moduledoc becomes Docstring and
// NOT a doc span: Elixir writes it INSIDE the module, whereas a doc span is an
// edit range every consumer assumes sits AHEAD of the declaration.
func (w *elixirWalk) scanBody(body *tsg.Node) (doc string, isTest bool) {
	if body == nil {
		return "", false
	}
	for _, c := range body.Children() {
		if w.callTarget(c) == "use" && strings.HasPrefix(w.argText(c, "alias"), "ExUnit.") {
			isTest = true
		}
		if doc == "" && w.attributeName(c) == "moduledoc" {
			doc = w.stringArg(childByType(c, "call", w.lang))
		}
	}
	return doc, isTest
}

// attributeName returns a `@…` module attribute's name, empty for anything else.
func (w *elixirWalk) attributeName(n *tsg.Node) string {
	if childByType(n, "@", w.lang) == nil {
		return ""
	}
	inner := n
	if c := childByType(n, "call", w.lang); c != nil {
		inner = c
	}
	if id := childByType(inner, "identifier", w.lang); id != nil {
		return id.Text(w.src)
	}
	return ""
}

// emit appends a node stamped with its byte and doc spans, linked to its parent.
func (w *elixirWalk) emit(n *tsg.Node, parent int64, kind topology.NodeKind, name, qualified, signature string) int64 {
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind: kind, Name: name, Qualified: qualified, Signature: signature,
		StartLine: line(n.StartPoint()), EndLine: line(n.EndPoint()),
		Language: "elixir", Path: w.path,
	}
	setSpan(&node, n)
	node.DocStartByte, node.DocEndByte = w.docSpan(n)
	w.nodes = append(w.nodes, node)
	if parent >= 0 {
		w.edges = append(w.edges, topology.Edge{FromID: parent, ToID: idx, Kind: topology.EdgeContains, Confidence: 1.0, Source: "extractor"})
	}
	return idx
}

// elixirDocAttrs are the attributes bound to the declaration below them by
// position alone, so a doc span must carry them along.
var elixirDocAttrs = map[string]bool{
	"doc": true, "spec": true, "impl": true, "deprecated": true, "since": true, "typedoc": true,
}

// docSpan returns the byte span of the run of `#` comments and documentation
// attributes sitting flush above decl.
//
// It is not docSpanBefore with a comment predicate, because the run to capture
// is not comments: Elixir documents with `@doc` and binds `@spec`/`@impl` by
// POSITION alone. Consumers treat a doc span as part of the symbol, so a move
// leaving the @spec behind re-attaches it to the next def — wrong contract.
func (w *elixirWalk) docSpan(decl *tsg.Node) (start, end int) {
	sibs := siblingsOf(decl)
	i := nodeIndexIn(sibs, decl)
	if i <= 0 {
		return 0, 0
	}
	next, first := decl.StartByte(), -1
	for j := i - 1; j >= 0; j-- {
		s := sibs[j]
		if s.Type(w.lang) != "comment" && !elixirDocAttrs[w.attributeName(s)] {
			break
		}
		if !commentFlushBefore(w.src, s, next) {
			break
		}
		first, next = j, s.StartByte()
	}
	if first < 0 {
		return 0, 0
	}
	return clampU32(sibs[first].StartByte()), clampU32(sibs[i-1].EndByte())
}

// callEdges is the second pass: intra-file call edges, attributed to the
// innermost enclosing def. The scope type is "call" — where a definition IS a
// call there is nothing else to key on — so defScopeName carries the
// distinction, and funcIdx has no empty-string entry, leaving every other call's
// scope unchanged. The def* prefix test suffices: every other def* macro's head
// is an alias, a list or a keyword list, none of which headParts names.
func (w *elixirWalk) callEdges(root *tsg.Node) {
	w.nameCounts = callableNameCounts(w.nodes)
	seen := map[[2]int64]bool{}
	defScopeName := func(n *tsg.Node) string {
		if !strings.HasPrefix(w.callTarget(n), "def") {
			return ""
		}
		name, _, _, _ := w.headParts(n)
		return name
	}
	walkCallSites(root, scopeByType(w.lang, w.funcIdx, defScopeName, "call"),
		func(n *tsg.Node, cur int64) {
			if cur < 0 || n.Type(w.lang) != "call" || w.headers[n.StartByte()] {
				return
			}
			target, ok := w.funcIdx[w.callTarget(n)]
			if !ok || target == cur {
				return
			}
			if key := [2]int64{cur, target}; !seen[key] {
				seen[key] = true
				w.edges = append(w.edges, heuristicCallEdge(cur, target, w.nodes, w.nameCounts))
			}
		})
}

// elixirQualify joins a dotted module path with a member name.
func elixirQualify(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// elixirLastSegment returns a dotted alias's final segment.
func elixirLastSegment(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}

// elixirFlatten collapses a multi-line head onto one line: Elixir wraps long
// heads freely, and a Signature with raw newlines renders badly.
func elixirFlatten(s string) string { return strings.Join(strings.Fields(s), " ") }
