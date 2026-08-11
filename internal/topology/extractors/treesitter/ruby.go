package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// RubyExtractor extracts Ruby symbols using the gotreesitter Ruby grammar.
//
// Concurrency: stateless after construction and safe for concurrent use; each
// Extract call borrows a parser from the shared per-grammar pool and returns it
// before returning, because gotreesitter parsers are not safe for concurrent
// reuse.
type RubyExtractor struct {
	lang lazyGrammar
}

// NewRuby returns a tree-sitter-backed Ruby extractor.
func NewRuby() *RubyExtractor {
	return &RubyExtractor{lang: lazyGrammar{load: grammars.RubyLanguage}}
}

func (e *RubyExtractor) Language() string { return "ruby" }

// Extensions covers the source forms worth indexing plus the two bare filenames
// Ruby projects rely on. `.gemspec`, `Gemfile` and `Rakefile` are ordinary Ruby
// that happens to be configuration, and a project's dependency and task
// definitions are exactly what an agent asks the Map for first.
func (e *RubyExtractor) Extensions() []string {
	return []string{".rb", ".rake", ".gemspec", "gemfile", "rakefile"}
}

// Extract parses src and returns Ruby's declarations: modules and classes as
// types, their methods (instance and singleton) as methods, top-level defs as
// functions, constant assignments as constants, attr_* declarations as fields,
// require/require_relative as imports, and both minitest `test_*` methods and
// RSpec describe/context/it blocks as tests — with certain (1.0) containment
// edges from each module or class to what it declares, and heuristic call edges
// between callables in the same file. Returns (nil, nil, nil) when src cannot be
// parsed.
func (e *RubyExtractor) Extract(ctx context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	lang := e.lang.get()
	return extractWith(ctx, lang, src, func(root *tsg.Node) ([]topology.Node, []topology.Edge) {
		w := &rubyWalk{lang: lang, src: src, path: relPath, funcIdx: map[string]int64{}}
		w.walk(root, -1, "", false)
		w.callEdges(root)
		return w.nodes, w.edges
	})
}

type rubyWalk struct {
	lang       *tsg.Language
	src        []byte
	path       string
	nodes      []topology.Node
	edges      []topology.Edge
	funcIdx    map[string]int64
	nameCounts map[string]int
}

// walk descends the tree carrying the enclosing module/class (its node index and
// qualified name) and whether it is inside a method body.
//
// inMethod is the locals-suppression flag, and it is the whole reason this is
// not a flat type switch. Ruby draws no syntactic distinction between a local
// assignment and a constant one — `total = 5` inside a method and `CONST = 42`
// at class level are both `assignment` nodes — so without the flag every local
// variable in the codebase would be emitted as a symbol. The pure-Go Swift walk
// shipped with exactly that defect and it took a parity sweep to find (134
// leaked locals), so it is guarded here by test rather than left to review.
func (w *rubyWalk) walk(n *tsg.Node, enclosing int64, prefix string, inMethod bool) {
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "module", "class":
			w.addScope(c, enclosing, prefix)
		case "method":
			w.addMethod(c, enclosing, prefix, false)
		case "singleton_method":
			w.addMethod(c, enclosing, prefix, true)
		case "assignment":
			// Only a constant assignment is a declaration; a local one is not,
			// and inside a method neither is.
			if !inMethod {
				w.addConstant(c, enclosing, prefix)
			}
		case "call":
			w.addCall(c, enclosing, prefix, inMethod)
		default:
			w.walk(c, enclosing, prefix, inMethod)
		}
	}
}

// addScope emits a module or class as a type and descends into its body with
// itself as the enclosing scope.
func (w *rubyWalk) addScope(n *tsg.Node, enclosing int64, prefix string) {
	name := w.scopeName(n)
	if name == "" {
		return
	}
	qualified := name
	if prefix != "" {
		qualified = prefix + "::" + name
	}
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      topology.KindType,
		Name:      name,
		Qualified: qualified,
		Signature: w.scopeSignature(n, name),
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "ruby",
		Path:      w.path,
	}
	setSpan(&node, n)
	node.DocStartByte, node.DocEndByte = docSpanBefore(n, w.lang, w.src, isRubyComment)
	w.nodes = append(w.nodes, node)
	w.link(enclosing, idx)
	w.walk(n, idx, qualified, false)
}

// scopeName reads a module/class name, which is a bare `constant` for
// `class Invoice` and a `scope_resolution` for `class MiniTest::Test`.
func (w *rubyWalk) scopeName(n *tsg.Node) string {
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "constant":
			return c.Text(w.src)
		case "scope_resolution":
			return strings.TrimSpace(c.Text(w.src))
		}
	}
	return ""
}

// scopeSignature reproduces the declaration head, so `class Invoice < Base`
// keeps its superclass — the one piece of a Ruby class line that carries
// structure worth searching for.
func (w *rubyWalk) scopeSignature(n *tsg.Node, name string) string {
	head := n.Type(w.lang) + " " + name
	if sup := childByType(n, "superclass", w.lang); sup != nil {
		head += " " + strings.Join(strings.Fields(sup.Text(w.src)), " ")
	}
	return head
}

// addMethod emits a def. A method inside a class or module is a method; a
// top-level def is a function, matching how the other extractors split the two.
//
// The qualified name follows Ruby's own notation rather than a dotted path:
// `Billing::Invoice#total` for an instance method and `Billing::Invoice.build`
// for a singleton one. That is what a Ruby developer (and a Ruby backtrace)
// writes, so it is what a search for it will contain.
func (w *rubyWalk) addMethod(n *tsg.Node, enclosing int64, prefix string, singleton bool) {
	name := w.methodName(n)
	if name == "" {
		return
	}
	qualified := name
	if prefix != "" {
		sep := "#"
		if singleton {
			sep = "."
		}
		qualified = prefix + sep + name
	}
	kind := topology.KindFunction
	if enclosing >= 0 {
		kind = topology.KindMethod
	}
	if isRubyTestMethod(name) {
		kind = topology.KindTest
	}
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: qualified,
		Signature: w.methodSignature(n, name, singleton),
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "ruby",
		Path:      w.path,
	}
	setSpan(&node, n)
	node.DocStartByte, node.DocEndByte = docSpanBefore(n, w.lang, w.src, isRubyComment)
	w.nodes = append(w.nodes, node)
	w.link(enclosing, idx)
	if _, seen := w.funcIdx[name]; !seen {
		w.funcIdx[name] = idx
	}
	// Descend with inMethod set so locals are suppressed, but keep scanning:
	// Ruby nests classes and defs inside method bodies freely.
	w.walk(n, enclosing, prefix, true)
}

// methodName reads the def's name. `def self.build` puts `self` ahead of the
// name, and operator methods (`def ==`) come through as an operator node rather
// than an identifier, so the name is the last non-parameter, non-body token.
func (w *rubyWalk) methodName(n *tsg.Node) string {
	var name string
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "identifier", "constant", "operator", "setter":
			if t := c.Text(w.src); t != "self" {
				name = t
			}
		case "method_parameters", "body_statement", "parameters":
			return name
		}
	}
	return name
}

func (w *rubyWalk) methodSignature(n *tsg.Node, name string, singleton bool) string {
	head := "def "
	if singleton {
		head += "self."
	}
	head += name
	if params := childByType(n, "method_parameters", w.lang); params != nil {
		head += strings.Join(strings.Fields(params.Text(w.src)), " ")
	}
	return head
}

// addConstant emits `CONST = value`. Ruby marks a constant by capitalisation
// alone, so the LHS node type (`constant` rather than `identifier`) is the
// grammar's own answer to the question and is used instead of re-deriving it
// from the spelling.
func (w *rubyWalk) addConstant(n *tsg.Node, enclosing int64, prefix string) {
	lhs := firstNamedChild(n)
	if lhs == nil || lhs.Type(w.lang) != "constant" {
		return
	}
	name := lhs.Text(w.src)
	qualified := name
	if prefix != "" {
		qualified = prefix + "::" + name
	}
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      topology.KindConstant,
		Name:      name,
		Qualified: qualified,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "ruby",
		Path:      w.path,
	}
	setSpan(&node, n)
	w.nodes = append(w.nodes, node)
	w.link(enclosing, idx)
}

// addCall handles the bare-word forms Ruby uses for declarations. `require`,
// `attr_reader` and `describe` are ordinary method calls syntactically, so the
// grammar offers no declaration node for any of them — but they declare a
// dependency, an accessor and a test respectively, and omitting them would leave
// a Rails or RSpec file looking almost empty.
func (w *rubyWalk) addCall(n *tsg.Node, enclosing int64, prefix string, inMethod bool) {
	switch w.callName(n) {
	case "require", "require_relative":
		w.addImport(n)
	case "attr_reader", "attr_writer", "attr_accessor":
		if !inMethod {
			w.addAttrs(n, enclosing, prefix)
		}
	case "describe", "context", "it", "specify", "feature", "scenario":
		if w.addSpec(n) {
			// The block body holds nested examples; keep descending, but the
			// spec node itself is the scope.
			w.walk(n, enclosing, prefix, inMethod)
			return
		}
	}
	w.walk(n, enclosing, prefix, inMethod)
}

// callName returns the method name of a call, ignoring any receiver, so both
// `describe` and `RSpec.describe` answer "describe".
func (w *rubyWalk) callName(n *tsg.Node) string {
	var last string
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "identifier":
			last = c.Text(w.src)
		case "argument_list", "do_block", "block":
			return last
		}
	}
	return last
}

func (w *rubyWalk) addImport(n *tsg.Node) {
	args := childByType(n, "argument_list", w.lang)
	if args == nil {
		return
	}
	name := w.firstStringArg(args)
	if name == "" {
		return
	}
	node := topology.Node{
		Kind:      topology.KindImport,
		Name:      name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "ruby",
		Path:      w.path,
	}
	setSpan(&node, n)
	w.nodes = append(w.nodes, node)
}

// addAttrs emits one field per symbol, so `attr_accessor :a, :b` yields two —
// these are the accessors a reader is looking for, and collapsing them into one
// node named after the first would be wrong rather than merely coarse.
func (w *rubyWalk) addAttrs(n *tsg.Node, enclosing int64, prefix string) {
	args := childByType(n, "argument_list", w.lang)
	if args == nil {
		return
	}
	for _, a := range args.Children() {
		if a.Type(w.lang) != "simple_symbol" {
			continue
		}
		name := strings.TrimPrefix(a.Text(w.src), ":")
		if name == "" {
			continue
		}
		qualified := name
		if prefix != "" {
			qualified = prefix + "#" + name
		}
		idx := int64(len(w.nodes))
		node := topology.Node{
			Kind:      topology.KindField,
			Name:      name,
			Qualified: qualified,
			StartLine: line(a.StartPoint()),
			EndLine:   line(a.EndPoint()),
			Language:  "ruby",
			Path:      w.path,
		}
		setSpan(&node, a)
		w.nodes = append(w.nodes, node)
		w.link(enclosing, idx)
	}
}

// addSpec emits an RSpec block as a test named by its description, and reports
// whether it did. Only a call carrying a block is a spec: `it` with no block is
// a pending example, and `describe` as a bare word inside a string is not a call
// at all.
func (w *rubyWalk) addSpec(n *tsg.Node) bool {
	if childByType(n, "do_block", w.lang) == nil && childByType(n, "block", w.lang) == nil {
		return false
	}
	args := childByType(n, "argument_list", w.lang)
	if args == nil {
		return false
	}
	name := w.firstStringArg(args)
	if name == "" {
		// `describe Invoice do` names a class rather than a string.
		if c := childByType(args, "constant", w.lang); c != nil {
			name = c.Text(w.src)
		}
	}
	if name == "" {
		return false
	}
	w.nodes = appendTest(w.nodes, name, "ruby", w.path, n)
	setSpan(&w.nodes[len(w.nodes)-1], n)
	return true
}

// firstStringArg returns the text of the first string argument with its quotes
// and interpolation markers stripped.
func (w *rubyWalk) firstStringArg(args *tsg.Node) string {
	for _, a := range args.Children() {
		if a.Type(w.lang) != "string" {
			continue
		}
		if content := childByType(a, "string_content", w.lang); content != nil {
			return content.Text(w.src)
		}
		return strings.Trim(a.Text(w.src), `"'`)
	}
	return ""
}

func (w *rubyWalk) link(parent, child int64) {
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

// callEdges is the second pass: intra-file call edges, attributed to the
// innermost enclosing def.
func (w *rubyWalk) callEdges(root *tsg.Node) {
	w.nameCounts = callableNameCounts(w.nodes)
	seen := map[[2]int64]bool{}
	walkCallSites(root,
		scopeByType(w.lang, w.funcIdx, w.methodName, "method", "singleton_method"),
		func(n *tsg.Node, cur int64) {
			if cur < 0 || n.Type(w.lang) != "call" {
				return
			}
			target, ok := w.funcIdx[w.callName(n)]
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

// isRubyTestMethod matches minitest's convention, where a test is any method
// named test_*. RSpec examples are blocks rather than defs and are handled by
// addSpec.
func isRubyTestMethod(name string) bool {
	return strings.HasPrefix(name, "test_")
}

func isRubyComment(typ string) bool { return typ == "comment" }
