package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// LuaExtractor extracts Lua symbols using the gotreesitter Lua grammar.
//
// Concurrency: stateless after construction and safe for concurrent use; each
// Extract call borrows a parser from the shared per-grammar pool and returns it,
// because gotreesitter parsers are not safe for concurrent reuse.
type LuaExtractor struct {
	lang lazyGrammar
}

// NewLua returns a tree-sitter-backed Lua extractor.
func NewLua() *LuaExtractor {
	return &LuaExtractor{lang: lazyGrammar{load: grammars.LuaLanguage}}
}

func (e *LuaExtractor) Language() string     { return "lua" }
func (e *LuaExtractor) Extensions() []string { return []string{".lua"} }

// Extract parses src and returns Lua's declarations: functions and methods in
// all four spellings the language allows, `require` as imports, and the test
// cases a busted/luaunit suite defines.
//
// Lua declares a function four ways, and an extractor that handles only the
// first misses most of a real module:
//
//	function greet(n)          -- global
//	local function helper(n)   -- file-local
//	function M.helper(n)       -- a field on a module table
//	function Account:deposit() -- a method, with an implicit self
//	local f = function(n)      -- and an anonymous one bound to a name
//
// The `M.helper` and `Account:deposit` forms are how nearly every Lua module is
// organised, so they carry their table into the qualified name — `M.helper`,
// `Account:deposit` — keeping the punctuation Lua itself uses, because the colon
// is what tells a reader the function takes an implicit self.
//
// Ordinary local variables are deliberately NOT emitted. A Lua file is mostly
// locals, they carry no structure, and emitting them buries the functions.
func (e *LuaExtractor) Extract(ctx context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	lang := e.lang.get()
	return extractWith(ctx, lang, src, func(root *tsg.Node) ([]topology.Node, []topology.Edge) {
		w := &luaWalk{lang: lang, src: src, path: relPath, funcIdx: map[string]int64{}, seenImport: map[string]bool{}}
		w.walk(root, -1, false)
		w.imports(root)
		w.callEdges(root)
		return w.nodes, w.edges
	})
}

type luaWalk struct {
	lang       *tsg.Language
	src        []byte
	path       string
	nodes      []topology.Node
	edges      []topology.Edge
	funcIdx    map[string]int64
	nameCounts map[string]int
	seenImport map[string]bool
}

// walk descends the chunk. parent is the enclosing function's node index (-1 at
// file scope) and inFunc says whether we are inside a function body, which is
// what makes a local a local.
func (w *luaWalk) walk(n *tsg.Node, parent int64, inFunc bool) {
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "function_declaration":
			w.addDeclaration(c, parent)
		case "variable_declaration", "assignment_statement":
			w.addAssigned(c, parent, inFunc)
		case "return_statement":
			// `return { go = function() … end }` is one of the two standard
			// module shapes and the table is never bound to a name, so nothing
			// else in this walk would reach it.
			if exprs := childByType(c, "expression_list", w.lang); exprs != nil {
				if tbl := childByType(exprs, "table_constructor", w.lang); tbl != nil {
					w.addTableFunctions(tbl, parent, "")
				}
			}
			w.walk(c, parent, inFunc)
		case "function_call":
			if idx := w.addTest(c, parent); idx >= 0 {
				w.walk(c, idx, true)
				continue
			}
			w.walk(c, parent, inFunc)
		default:
			w.walk(c, parent, inFunc)
		}
	}
}

// addDeclaration handles the `function name()` family, including the dotted and
// colon forms.
func (w *luaWalk) addDeclaration(n *tsg.Node, parent int64) {
	name, qualified, kind := w.declName(n)
	if name == "" {
		return
	}
	if isLuaTestName(name) {
		kind = topology.KindTest
	}
	idx := w.emit(n, parent, kind, name, qualified, w.signature(n, qualified))
	if body := childByType(n, "block", w.lang); body != nil {
		w.walk(body, idx, true)
	}
}

// declName returns a declaration's short name, its qualified name and the kind
// implied by how it was written.
//
// The colon form is a method and the dot form is a plain function on a table:
// Lua's own syntax makes that distinction, and it is a real one, because only
// the colon form receives an implicit self.
func (w *luaWalk) declName(n *tsg.Node) (name, qualified string, kind topology.NodeKind) {
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "identifier":
			t := c.Text(w.src)
			return t, t, topology.KindFunction
		case "dot_index_expression":
			recv, field := w.indexParts(c)
			if field == "" {
				return "", "", topology.KindFunction
			}
			return field, joinLua(recv, ".", field), topology.KindFunction
		case "method_index_expression":
			recv, field := w.indexParts(c)
			if field == "" {
				return "", "", topology.KindMethod
			}
			return field, joinLua(recv, ":", field), topology.KindMethod
		}
	}
	return "", "", topology.KindFunction
}

// indexParts splits `M.helper` or `Account:deposit` into its receiver and field.
// A chained receiver (`a.b.c`) keeps its full text, so the qualified name stays
// the path someone would actually type.
func (w *luaWalk) indexParts(n *tsg.Node) (recv, field string) {
	kids := n.Children()
	var named []*tsg.Node
	for _, c := range kids {
		if c.IsNamed() {
			named = append(named, c)
		}
	}
	if len(named) < 2 {
		return "", ""
	}
	return named[0].Text(w.src), named[len(named)-1].Text(w.src)
}

// addAssigned handles `local f = function() … end` and the table-literal form,
// which is how a module built as `local M = { go = function() … end }` declares
// its functions.
//
// A plain local (`local n = 1`) is skipped: a Lua file is mostly locals, they
// carry no structure, and emitting them buries the functions.
func (w *luaWalk) addAssigned(n *tsg.Node, parent int64, inFunc bool) {
	assign := n
	if assign.Type(w.lang) == "variable_declaration" {
		assign = childByType(n, "assignment_statement", w.lang)
		if assign == nil {
			return
		}
	}
	vars := childByType(assign, "variable_list", w.lang)
	exprs := childByType(assign, "expression_list", w.lang)
	if vars == nil || exprs == nil {
		return
	}
	names := namedChildren(vars)
	values := namedChildren(exprs)
	for i, v := range values {
		if i >= len(names) {
			break
		}
		target := names[i].Text(w.src)
		if target == "" {
			continue
		}
		switch v.Type(w.lang) {
		case "function_definition":
			// A function bound to a local name inside another function is still
			// a callable worth naming, so it is emitted with the enclosing
			// function as its parent rather than dropped.
			idx := w.emit(n, parent, topology.KindFunction, lastLuaSegment(target), target, w.signature(v, target))
			if body := childByType(v, "block", w.lang); body != nil {
				w.walk(body, idx, true)
			}
		case "table_constructor":
			if inFunc {
				continue
			}
			w.addTableFunctions(v, parent, target)
		}
	}
}

// addTableFunctions emits the functions declared inside a table literal, which
// is one of the two common module shapes.
func (w *luaWalk) addTableFunctions(tbl *tsg.Node, parent int64, owner string) {
	for _, f := range tbl.Children() {
		if f.Type(w.lang) != "field" {
			continue
		}
		named := namedChildren(f)
		if len(named) < 2 {
			continue
		}
		key := named[0].Text(w.src)
		if key == "" {
			continue
		}
		value := named[len(named)-1]
		if value.Type(w.lang) == "table_constructor" {
			// A table of tables is how a plugin groups related helpers; the
			// nesting belongs in the qualified name.
			w.addTableFunctions(value, parent, joinLua(owner, ".", key))
			continue
		}
		if value.Type(w.lang) != "function_definition" {
			continue
		}
		fn := value
		qualified := joinLua(owner, ".", key)
		idx := w.emit(f, parent, topology.KindFunction, key, qualified, w.signature(fn, qualified))
		if body := childByType(fn, "block", w.lang); body != nil {
			w.walk(body, idx, true)
		}
	}
}

// addTest emits a busted-style `it("…", function() … end)` case, returning its
// index or -1. `describe` is a grouping call rather than a case, so it becomes a
// section that the cases inside it hang off.
func (w *luaWalk) addTest(call *tsg.Node, parent int64) int64 {
	callee := w.callName(call)
	kind, ok := luaTestCall(callee)
	if !ok {
		return -1
	}
	args := childByType(call, "arguments", w.lang)
	if args == nil {
		return -1
	}
	str := childByType(args, "string", w.lang)
	if str == nil {
		return -1
	}
	name := strings.TrimSpace(stringContent(str, w.lang, w.src))
	if name == "" {
		return -1
	}
	return w.emit(call, parent, kind, name, name, callee+"("+quoteLua(name)+")")
}

// imports records every `require "mod"` in the file, at any depth: Lua modules
// are routinely required inside a function, and a lazily-required dependency is
// still a dependency.
func (w *luaWalk) imports(n *tsg.Node) {
	for _, c := range n.Children() {
		if c.Type(w.lang) == "function_call" && w.callName(c) == "require" {
			if mod := w.requireArg(c); mod != "" && !w.seenImport[mod] {
				w.seenImport[mod] = true
				node := topology.Node{
					Kind:      topology.KindImport,
					Name:      mod,
					Qualified: mod,
					StartLine: line(c.StartPoint()),
					EndLine:   line(c.EndPoint()),
					Language:  "lua",
					Path:      w.path,
				}
				setSpan(&node, c)
				w.nodes = append(w.nodes, node)
			}
		}
		w.imports(c)
	}
}

// requireArg returns the module name a require call names, for both the
// parenthesised and the bare-string spellings Lua allows.
func (w *luaWalk) requireArg(call *tsg.Node) string {
	args := childByType(call, "arguments", w.lang)
	if args == nil {
		return ""
	}
	if str := childByType(args, "string", w.lang); str != nil {
		return stringContent(str, w.lang, w.src)
	}
	return ""
}

// callEdges links a call site to the function it names, once the whole file has
// been walked and every name is known.
func (w *luaWalk) callEdges(root *tsg.Node) {
	w.nameCounts = callableNameCounts(w.nodes)
	seen := map[[2]int64]bool{}
	walkCallSites(root,
		scopeByType(w.lang, w.funcIdx, w.scopeName, "function_declaration", "function_definition"),
		func(n *tsg.Node, cur int64) {
			if cur < 0 || n.Type(w.lang) != "function_call" {
				return
			}
			name := w.callName(n)
			if name == "" || name == "require" {
				return
			}
			target, ok := w.funcIdx[lastLuaSegment(name)]
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

// scopeName names the function a call site sits inside, so scopeByType can map
// it back to the node this walk already emitted.
func (w *luaWalk) scopeName(n *tsg.Node) string {
	if n.Type(w.lang) == "function_declaration" {
		name, _, _ := w.declName(n)
		return name
	}
	return ""
}

// callName returns the callee as written: `helper`, `M.helper` or `obj:method`.
func (w *luaWalk) callName(call *tsg.Node) string {
	for _, c := range call.Children() {
		switch c.Type(w.lang) {
		case "identifier":
			return c.Text(w.src)
		case "dot_index_expression", "method_index_expression":
			_, field := w.indexParts(c)
			return field
		}
	}
	return ""
}

func (w *luaWalk) signature(n *tsg.Node, qualified string) string {
	params := childByType(n, "parameters", w.lang)
	if params == nil {
		return "function " + qualified + "()"
	}
	return "function " + qualified + normaliseSpace(params.Text(w.src))
}

func (w *luaWalk) emit(n *tsg.Node, parent int64, kind topology.NodeKind, name, qualified, sig string) int64 {
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: qualified,
		Signature: sig,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "lua",
		Path:      w.path,
	}
	setSpan(&node, n)
	node.DocStartByte, node.DocEndByte = docSpanBefore(n, w.lang, w.src, isLuaComment)
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
	// First definition wins, matching the other extractors: funcIdx keeps one
	// index per name so an ambiguous call can be down-weighted rather than
	// pointed at an arbitrary redefinition.
	if _, seen := w.funcIdx[name]; !seen && (kind == topology.KindFunction || kind == topology.KindMethod) {
		w.funcIdx[name] = idx
	}
	return idx
}

// namedChildren is the named-node subset of a node's children, which is what
// every positional read in this walk actually wants.
func namedChildren(n *tsg.Node) []*tsg.Node {
	var out []*tsg.Node
	for _, c := range n.Children() {
		if c.IsNamed() {
			out = append(out, c)
		}
	}
	return out
}

// stringContent unwraps a Lua string literal, which the grammar exposes as a
// string_content child rather than as the quoted text.
func stringContent(str *tsg.Node, lang *tsg.Language, src []byte) string {
	if c := childByType(str, "string_content", lang); c != nil {
		return c.Text(src)
	}
	return strings.Trim(str.Text(src), `"'`)
}

// luaTestCall recognises the busted/luaunit vocabulary. `describe` groups and so
// becomes a section; `it`, `test` and `spec` are the cases themselves.
func luaTestCall(callee string) (topology.NodeKind, bool) {
	switch callee {
	case "describe", "context":
		return topology.KindSection, true
	case "it", "test", "spec":
		return topology.KindTest, true
	}
	return "", false
}

// isLuaTestName covers the xUnit-style suites that name test functions rather
// than passing them to a runner.
func isLuaTestName(name string) bool {
	return strings.HasPrefix(name, "test_") || strings.HasPrefix(name, "Test")
}

// lastLuaSegment reduces `a.b.c` to `c`, so a call written against a module
// alias still matches the function this file declared.
func lastLuaSegment(name string) string {
	if i := strings.LastIndexAny(name, ".:"); i >= 0 {
		return name[i+1:]
	}
	return name
}

func joinLua(recv, sep, field string) string {
	if recv == "" {
		return field
	}
	return recv + sep + field
}

func quoteLua(s string) string { return `"` + s + `"` }

// isLuaComment names the grammar's comment node. Lua spells both the line and
// the long-bracket form `comment`, so one case covers them.
func isLuaComment(typ string) bool { return typ == "comment" }
