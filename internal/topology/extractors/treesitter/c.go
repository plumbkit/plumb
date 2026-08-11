package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// CExtractor extracts C symbols using the gotreesitter C grammar.
//
// Concurrency: stateless after construction and safe for concurrent use; each
// Extract call borrows a parser from the shared per-grammar pool and returns it
// before returning, because gotreesitter parsers are not safe for concurrent
// reuse.
type CExtractor struct {
	lang lazyGrammar
}

// NewC returns a tree-sitter-backed C extractor.
func NewC() *CExtractor {
	return &CExtractor{lang: lazyGrammar{load: grammars.CLanguage}}
}

func (e *CExtractor) Language() string     { return "c" }
func (e *CExtractor) Extensions() []string { return []string{".c", ".h"} }

// Extract parses src and returns C's declarations: function definitions and
// prototypes as functions, structs, unions, enums and typedefs as types, their
// members as fields, enumerators and top-level const/`#define` values as
// constants, and `#include` as imports — with certain (1.0) containment edges
// from an aggregate to its members and heuristic call edges between functions in
// the same file. Returns (nil, nil, nil) when src cannot be parsed.
func (e *CExtractor) Extract(ctx context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	lang := e.lang.get()
	return extractWith(ctx, lang, src, func(root *tsg.Node) ([]topology.Node, []topology.Edge) {
		w := &cWalk{lang: lang, src: src, path: relPath, langName: "c", funcIdx: map[string]int64{}, defined: map[string]bool{}}
		w.walk(root)
		w.flushPrototypes()
		w.callEdges(root)
		return w.nodes, w.edges
	})
}

// cWalk is shared with the C++ and Objective-C extractors, which are supersets
// of C: langName keeps each one's nodes stamped with its own language, and the
// subset/superset relationship means a construct handled here is handled there
// for free.
type cWalk struct {
	lang     *tsg.Language
	src      []byte
	path     string
	langName string
	nodes    []topology.Node
	edges    []topology.Edge
	funcIdx  map[string]int64
	// defined records which functions have a body in this file, so a prototype
	// for one of them is dropped rather than emitted as a second symbol.
	defined map[string]bool
	// prototypes are held back until the walk finishes, because whether to keep
	// one depends on a definition that may appear later in the file.
	prototypes []topology.Node
	nameCounts map[string]int
}

func (w *cWalk) walk(root *tsg.Node) {
	for _, n := range root.Children() {
		w.top(n)
	}
}

// top handles one top-level construct. Nested compound statements are never
// descended into from here, which is what suppresses locals: a `declaration`
// inside a function body looks identical to a file-scope one.
func (w *cWalk) top(n *tsg.Node) {
	switch n.Type(w.lang) {
	case "preproc_include":
		w.addInclude(n)
	case "preproc_def":
		w.addMacro(n, topology.KindConstant)
	case "preproc_function_def":
		w.addMacro(n, topology.KindFunction)
	case "type_definition":
		w.addTypedef(n)
	case "struct_specifier", "union_specifier", "enum_specifier":
		w.addAggregate(n, "")
	case "function_definition":
		w.addFunction(n)
	case "declaration":
		w.addDeclaration(n)
	case "preproc_if", "preproc_ifdef", "preproc_else", "preproc_elif", "linkage_specification", "declaration_list":
		// Conditional compilation and `extern "C" { … }` wrap ordinary
		// declarations. Descending keeps a header guarded by #ifndef from
		// indexing as empty, which would otherwise hide most of a C codebase.
		for _, c := range n.Children() {
			w.top(c)
		}
	}
}

func (w *cWalk) addInclude(n *tsg.Node) {
	var name string
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "system_lib_string":
			name = strings.Trim(c.Text(w.src), "<>")
		case "string_literal":
			if content := childByType(c, "string_content", w.lang); content != nil {
				name = content.Text(w.src)
			} else {
				name = strings.Trim(c.Text(w.src), `"`)
			}
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
		Language:  w.langName,
		Path:      w.path,
	}
	setSpan(&node, n)
	w.nodes = append(w.nodes, node)
}

// addMacro records `#define`. An object-like macro is a constant; a
// function-like one is emitted as a function, because that is how it is called
// and how someone searching for it thinks of it.
func (w *cWalk) addMacro(n *tsg.Node, kind topology.NodeKind) {
	name := ""
	if id := childByType(n, "identifier", w.lang); id != nil {
		name = id.Text(w.src)
	}
	if name == "" {
		return
	}
	sig := "#define " + name
	if params := childByType(n, "preproc_params", w.lang); params != nil {
		sig += params.Text(w.src)
	}
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		Signature: sig,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  w.langName,
		Path:      w.path,
	}
	setSpan(&node, n)
	node.DocStartByte, node.DocEndByte = docSpanBefore(n, w.lang, w.src, isCComment)
	w.nodes = append(w.nodes, node)
	if kind == topology.KindFunction {
		w.noteFunc(name, idx)
	}
}

// addTypedef emits the typedef's own name and, when it wraps an aggregate, that
// aggregate's members. `typedef struct { … } Point;` is the idiomatic C spelling
// and has no name on the struct at all, so the typedef name is the only one a
// reader ever uses.
func (w *cWalk) addTypedef(n *tsg.Node) {
	var name string
	for _, c := range n.Children() {
		if c.Type(w.lang) == "type_identifier" {
			name = c.Text(w.src) // the LAST one is the typedef's name
		}
	}
	if name == "" {
		return
	}
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      topology.KindType,
		Name:      name,
		Qualified: name,
		Signature: "typedef " + name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  w.langName,
		Path:      w.path,
	}
	setSpan(&node, n)
	node.DocStartByte, node.DocEndByte = docSpanBefore(n, w.lang, w.src, isCComment)
	w.nodes = append(w.nodes, node)
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "struct_specifier", "union_specifier":
			w.addMembers(c, idx)
		case "enum_specifier":
			w.addEnumerators(c, idx)
		}
	}
}

// addAggregate emits a named struct/union/enum. An anonymous one reached at file
// scope belongs to a typedef, which has already emitted it.
func (w *cWalk) addAggregate(n *tsg.Node, prefix string) {
	id := childByType(n, "type_identifier", w.lang)
	if id == nil {
		return
	}
	name := id.Text(w.src)
	qualified := name
	if prefix != "" {
		qualified = prefix + "::" + name
	}
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      topology.KindType,
		Name:      name,
		Qualified: qualified,
		Signature: strings.TrimSuffix(n.Type(w.lang), "_specifier") + " " + name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  w.langName,
		Path:      w.path,
	}
	setSpan(&node, n)
	node.DocStartByte, node.DocEndByte = docSpanBefore(n, w.lang, w.src, isCComment)
	w.nodes = append(w.nodes, node)
	if n.Type(w.lang) == "enum_specifier" {
		w.addEnumerators(n, idx)
		return
	}
	w.addMembers(n, idx)
}

func (w *cWalk) addMembers(spec *tsg.Node, parent int64) {
	list := childByType(spec, "field_declaration_list", w.lang)
	if list == nil {
		return
	}
	for _, f := range list.Children() {
		if f.Type(w.lang) != "field_declaration" {
			continue
		}
		name := w.declaratorName(f)
		if name == "" {
			continue
		}
		idx := int64(len(w.nodes))
		node := topology.Node{
			Kind:      topology.KindField,
			Name:      name,
			Qualified: w.qualify(parent, name),
			Signature: strings.Join(strings.Fields(f.Text(w.src)), " "),
			StartLine: line(f.StartPoint()),
			EndLine:   line(f.EndPoint()),
			Language:  w.langName,
			Path:      w.path,
		}
		setSpan(&node, f)
		w.nodes = append(w.nodes, node)
		w.link(parent, idx)
	}
}

func (w *cWalk) addEnumerators(spec *tsg.Node, parent int64) {
	list := childByType(spec, "enumerator_list", w.lang)
	if list == nil {
		return
	}
	seen := map[string]bool{}
	for _, e := range list.Children() {
		if e.Type(w.lang) != "enumerator" {
			continue
		}
		id := childByType(e, "identifier", w.lang)
		if id == nil {
			continue
		}
		name := id.Text(w.src)
		seen[name] = true
		idx := int64(len(w.nodes))
		node := topology.Node{
			Kind:      topology.KindConstant,
			Name:      name,
			Qualified: w.qualify(parent, name),
			StartLine: line(e.StartPoint()),
			EndLine:   line(e.EndPoint()),
			Language:  w.langName,
			Path:      w.path,
		}
		setSpan(&node, e)
		w.nodes = append(w.nodes, node)
		w.link(parent, idx)
	}
	if hasMissingOrError(list) {
		w.recoverEnumerators(list, parent, seen)
	}
}

func (w *cWalk) addFunction(n *tsg.Node) {
	decl := w.findDeclarator(n)
	if decl == nil {
		return
	}
	name := w.declaratorName(decl)
	if name == "" {
		return
	}
	w.defined[name] = true
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      topology.KindFunction,
		Name:      name,
		Qualified: name,
		Signature: w.signature(n, decl),
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  w.langName,
		Path:      w.path,
	}
	setSpan(&node, n)
	node.DocStartByte, node.DocEndByte = docSpanBefore(n, w.lang, w.src, isCComment)
	w.nodes = append(w.nodes, node)
	w.noteFunc(name, idx)
}

// addDeclaration covers file-scope `declaration` nodes, which C uses for two
// unrelated things: a function prototype and a variable/constant definition.
func (w *cWalk) addDeclaration(n *tsg.Node) {
	if decl := childByType(n, "function_declarator", w.lang); decl != nil {
		w.addPrototype(n, decl)
		return
	}
	// A file-scope variable. `const` makes it a constant; everything else is a
	// variable, which is the distinction a reader cares about.
	name := w.declaratorName(n)
	if name == "" {
		return
	}
	kind := topology.KindVariable
	if childByType(n, "type_qualifier", w.lang) != nil &&
		strings.Contains(n.Text(w.src), "const") {
		kind = topology.KindConstant
	}
	node := topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		Signature: strings.Join(strings.Fields(n.Text(w.src)), " "),
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  w.langName,
		Path:      w.path,
	}
	setSpan(&node, n)
	w.nodes = append(w.nodes, node)
}

// addPrototype holds a declaration back rather than emitting it.
//
// A header is nothing but prototypes, so dropping them would index every .h file
// as empty — which is most of a C API's surface. But a .c file routinely
// declares and then defines the same function, and emitting both would report
// two symbols where the reader sees one. Since the definition can appear after
// the prototype, the decision cannot be made until the walk is finished.
func (w *cWalk) addPrototype(n, decl *tsg.Node) {
	name := w.declaratorName(decl)
	if name == "" {
		return
	}
	node := topology.Node{
		Kind:      topology.KindFunction,
		Name:      name,
		Qualified: name,
		Signature: strings.Join(strings.Fields(n.Text(w.src)), " "),
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  w.langName,
		Path:      w.path,
	}
	setSpan(&node, n)
	node.DocStartByte, node.DocEndByte = docSpanBefore(n, w.lang, w.src, isCComment)
	w.prototypes = append(w.prototypes, node)
}

// flushPrototypes appends the prototypes that no definition in this file
// supersedes. Appending after the walk keeps every index already handed to an
// edge valid, since nothing before them moves.
func (w *cWalk) flushPrototypes() {
	for _, p := range w.prototypes {
		if w.defined[p.Name] {
			continue
		}
		idx := int64(len(w.nodes))
		w.nodes = append(w.nodes, p)
		w.noteFunc(p.Name, idx)
	}
}

// findDeclarator returns the function_declarator of a definition, looking
// through the pointer wrappers a returned-pointer signature adds.
func (w *cWalk) findDeclarator(n *tsg.Node) *tsg.Node {
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "function_declarator":
			return c
		case "pointer_declarator":
			if d := w.findDeclarator(c); d != nil {
				return d
			}
		}
	}
	return nil
}

// declaratorName digs out the identifier a declarator eventually names.
//
// C wraps the name in one node per piece of type syntax, so `char *argv[]`
// arrives as array_declarator(pointer_declarator(identifier)). Matching only the
// outermost node would miss every pointer and array declaration — which in
// practice is most of the interesting ones.
func (w *cWalk) declaratorName(n *tsg.Node) string {
	switch n.Type(w.lang) {
	case "identifier", "field_identifier", "type_identifier":
		return n.Text(w.src)
	}
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "identifier", "field_identifier":
			return c.Text(w.src)
		case "pointer_declarator", "array_declarator", "function_declarator",
			"parenthesized_declarator", "init_declarator", "reference_declarator":
			if name := w.declaratorName(c); name != "" {
				return name
			}
		}
	}
	return ""
}

// signature reproduces the declaration head — return type plus the declarator —
// without the body, which is what an outline wants to show.
func (w *cWalk) signature(def, decl *tsg.Node) string {
	end := decl.EndByte()
	start := def.StartByte()
	if end <= start || int(end) > len(w.src) {
		return ""
	}
	return strings.Join(strings.Fields(string(w.src[start:end])), " ")
}

func (w *cWalk) qualify(parent int64, name string) string {
	if parent < 0 || int(parent) >= len(w.nodes) {
		return name
	}
	return w.nodes[parent].Qualified + "." + name
}

func (w *cWalk) noteFunc(name string, idx int64) {
	if _, seen := w.funcIdx[name]; !seen {
		w.funcIdx[name] = idx
	}
}

func (w *cWalk) link(parent, child int64) {
	if parent < 0 {
		return
	}
	w.edges = append(w.edges, topology.Edge{
		FromID:     parent,
		ToID:       child,
		Kind:       topology.EdgeContains,
		Confidence: 1.0,
		Source:     "extractor",
	})
}

// callEdges attributes each call expression to the function whose body contains
// it.
func (w *cWalk) callEdges(root *tsg.Node) {
	w.nameCounts = callableNameCounts(w.nodes)
	seen := map[[2]int64]bool{}
	enclosing := func(n *tsg.Node, cur int64) int64 {
		if n.Type(w.lang) != "function_definition" {
			return cur
		}
		decl := w.findDeclarator(n)
		if decl == nil {
			return cur
		}
		if idx, ok := w.funcIdx[w.declaratorName(decl)]; ok {
			return idx
		}
		return cur
	}
	walkCallSites(root, enclosing, func(n *tsg.Node, cur int64) {
		if cur < 0 || n.Type(w.lang) != "call_expression" {
			return
		}
		callee := firstNamedChild(n)
		if callee == nil {
			return
		}
		target, ok := w.funcIdx[callee.Text(w.src)]
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

// isCComment covers both spellings the C-family grammars emit.
func isCComment(typ string) bool { return typ == "comment" || typ == "block_comment" }
