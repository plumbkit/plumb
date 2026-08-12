package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// SCSSExtractor extracts SCSS landmarks using the gotreesitter SCSS grammar.
//
// Concurrency: stateless after construction and safe for concurrent use; each
// Extract call borrows a parser from the shared per-grammar pool and returns it,
// because gotreesitter parsers are not safe for concurrent reuse.
type SCSSExtractor struct {
	lang lazyGrammar
}

// NewSCSS returns a tree-sitter-backed SCSS extractor.
func NewSCSS() *SCSSExtractor {
	return &SCSSExtractor{lang: lazyGrammar{load: grammars.ScssLanguage}}
}

func (e *SCSSExtractor) Language() string     { return "scss" }
func (e *SCSSExtractor) Extensions() []string { return []string{".scss"} }

// Extract parses src and returns the landmarks someone navigates a Sass
// codebase by: `@mixin` and `@function` definitions with their parameter lists,
// rule sets (including `%placeholder` definitions) nested as containment,
// `@use`/`@forward`/`@import` as imports, top-level `$variables` as the design
// tokens they are, and `@include` as a call edge onto the mixin it invokes.
//
// Two things separate this from CSSExtractor rather than reusing it wholesale.
// Nesting is the first: in CSS a rule set is a leaf, so cssWalk never descends
// into one, while in SCSS the nested rule is the idiom and dropping it would
// lose most of the file. Mixins are the second: they give SCSS something CSS has
// no equivalent of — a named, parameterised, *called* unit — so SCSS earns
// function nodes and call edges where CSS has neither.
//
// Ordinary declarations (`color: red`) are deliberately NOT emitted, for the
// reason CSSExtractor records: a stylesheet has orders of magnitude more
// declarations than selectors and they bury the structure. Custom properties
// (`--brand`) remain the exception and are emitted by the shared cssWalk.
func (e *SCSSExtractor) Extract(ctx context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	lang := e.lang.get()
	return extractWith(ctx, lang, src, func(root *tsg.Node) ([]topology.Node, []topology.Edge) {
		w := &scssWalk{
			cssWalk:   cssWalk{lang: lang, src: src, path: relPath, langName: "scss"},
			callables: map[string]int64{},
		}
		w.walk(root, -1, "", true)
		return w.finish()
	})
}

// scssWalk embeds cssWalk to share the leaf emitters and the single node/edge
// buffer, and re-owns exactly the three methods that recurse.
//
// Re-owning them is not duplication for its own sake: Go has no virtual
// dispatch, so cssWalk.addSection calls *cssWalk's* walk. Delegating an
// `@media` block to it would dispatch every SCSS construct nested under the
// query through the CSS switch, which has no case for mixins, includes or
// nested rules — so they would vanish silently rather than fail loudly. The
// same trap applies to rule sets, which cssWalk deliberately treats as leaves.
type scssWalk struct {
	cssWalk
	// callables maps a mixin or function name to its node index, so an
	// `@include` seen anywhere in the file can be resolved after the walk.
	callables map[string]int64
	pending   []pendingInclude
}

// pendingInclude is an `@include` site held until the whole file is walked: a
// mixin may be defined below its first use, or in a file included earlier.
type pendingInclude struct {
	from int64
	name string
}

// walk descends one level, carrying the enclosing symbol (parent), its
// qualified name (prefix), and whether this level is still file scope.
//
// global is the locals-suppression flag. Sass scopes a `$variable` to the block
// that declares it, so only a file-scope variable is the shared design token
// worth a symbol; one inside a mixin or rule body is a local and is dropped.
// Control directives keep the flag, because `@if`/`@each` do not open a Sass
// variable scope — a `$v` inside a top-level `@each` really is global.
func (w *scssWalk) walk(n *tsg.Node, parent int64, prefix string, global bool) {
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "use_statement", "forward_statement", "import_statement":
			w.addImport(c)
		case "mixin_statement":
			w.addCallable(c, parent, prefix, "@mixin")
		case "function_statement":
			w.addCallable(c, parent, prefix, "@function")
		case "rule_set":
			w.addRule(c, parent, prefix)
		case "media_statement", "supports_statement":
			w.addSection(c, parent, prefix)
		case "keyframes_statement":
			w.addKeyframes(c, parent)
		case "include_statement":
			w.addInclude(c, parent, prefix)
		case "declaration":
			if global {
				w.addVariable(c, parent, prefix)
			}
		case "if_statement", "else_clause", "each_statement", "for_statement", "while_statement":
			// Emit nothing for the directive itself — `@each` is control flow,
			// not a landmark — but descend, because generating rules and mixins
			// inside one is ordinary Sass and those ARE landmarks.
			w.walk(c, parent, prefix, global)
		case "block":
			// Reached only via a control directive, whose block hangs directly
			// off the statement rather than off a named symbol.
			w.walk(c, parent, prefix, global)
		case "ERROR":
			// Keep walking through a recovery node. The grammar mis-parses
			// several ordinary Sass constructs, and where it does the ERROR it
			// produces often spans the rest of the file — but the mixins and
			// rules inside it are still parsed correctly as their own nodes.
			// Descending collects those; it does NOT invent anything, because
			// every symbol still has to come from a real typed node.
			w.walk(c, parent, prefix, global)
		}
	}
}

// addCallable emits an `@mixin` or `@function` definition, which are the only
// named, parameterised, callable units SCSS has and therefore the closest thing
// a stylesheet has to a function.
func (w *scssWalk) addCallable(n *tsg.Node, parent int64, prefix, keyword string) {
	id := childByType(n, "identifier", w.lang)
	if id == nil {
		return
	}
	name := id.Text(w.src)
	if name == "" {
		return
	}
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      topology.KindFunction,
		Name:      name,
		Qualified: qualifyCSS(prefix, name),
		Signature: keyword + " " + name + w.paramsOf(n),
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  w.langName,
		Path:      w.path,
	}
	setSpan(&node, n)
	node.DocStartByte, node.DocEndByte = docSpanBefore(n, w.lang, w.src, isSCSSComment)
	w.nodes = append(w.nodes, node)
	w.link(parent, idx)
	// First definition wins, matching how the other extractors keep one index
	// per name; a redefinition is rare and the later one is usually a variant
	// guarded by a control directive.
	if _, seen := w.callables[name]; !seen {
		w.callables[name] = idx
	}
	if block := childByType(n, "block", w.lang); block != nil {
		w.addCustomProperties(block, idx, node.Qualified)
		w.walk(block, idx, node.Qualified, false)
	}
}

// addRule emits a rule set named by its selector list and then descends, which
// is the whole difference from CSS: `.card { .title { … } }` is two symbols in
// SCSS and one in CSS.
//
// A `%placeholder` definition arrives here too — the grammar parses `%btn { … }`
// as an ordinary rule_set whose selector is a placeholder — so it needs no case
// of its own and keeps its `%` in the name, which is how anyone searches for it.
func (w *scssWalk) addRule(n *tsg.Node, parent int64, prefix string) {
	sel := childByType(n, "selectors", w.lang)
	if sel == nil {
		return
	}
	name := normaliseSpace(sel.Text(w.src))
	if name == "" {
		return
	}
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      topology.KindType,
		Name:      name,
		Qualified: qualifyCSS(prefix, name),
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  w.langName,
		Path:      w.path,
	}
	setSpan(&node, n)
	node.DocStartByte, node.DocEndByte = docSpanBefore(n, w.lang, w.src, isSCSSComment)
	w.nodes = append(w.nodes, node)
	w.link(parent, idx)
	if block := childByType(n, "block", w.lang); block != nil {
		w.addCustomProperties(block, idx, node.Qualified)
		w.walk(block, idx, node.Qualified, false)
	}
}

// addSection emits an `@media` / `@supports` block and descends into it.
//
// This shadows cssWalk.addSection deliberately; see the type's doc comment for
// why delegating the recursion would silently drop nested SCSS.
func (w *scssWalk) addSection(n *tsg.Node, parent int64, prefix string) {
	name := normaliseSpace(w.headOf(n))
	if name == "" {
		return
	}
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      topology.KindSection,
		Name:      name,
		Qualified: qualifyCSS(prefix, name),
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  w.langName,
		Path:      w.path,
	}
	setSpan(&node, n)
	w.nodes = append(w.nodes, node)
	w.link(parent, idx)
	if block := childByType(n, "block", w.lang); block != nil {
		w.walk(block, idx, node.Qualified, false)
	}
}

// addInclude records an `@include` as a call site and descends into the content
// block a caller may pass with it.
//
// The site is held rather than resolved on the spot because a mixin is
// routinely defined after — or, in a multi-file Sass project, above — its
// first use.
func (w *scssWalk) addInclude(n *tsg.Node, parent int64, prefix string) {
	if id := childByType(n, "identifier", w.lang); id != nil && parent >= 0 {
		if name := id.Text(w.src); name != "" {
			w.pending = append(w.pending, pendingInclude{from: parent, name: name})
		}
	}
	if block := childByType(n, "block", w.lang); block != nil {
		w.walk(block, parent, prefix, false)
	}
}

// addVariable emits a file-scope `$token: value` declaration as a constant, the
// same role CSSExtractor gives a custom property: a named value with few
// instances that someone searches for by name.
func (w *scssWalk) addVariable(d *tsg.Node, parent int64, prefix string) {
	name := leadingSassVariable(d.Text(w.src))
	if name == "" {
		return
	}
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      topology.KindConstant,
		Name:      name,
		Qualified: qualifyCSS(prefix, name),
		Signature: normaliseSpace(d.Text(w.src)),
		StartLine: line(d.StartPoint()),
		EndLine:   line(d.EndPoint()),
		Language:  w.langName,
		Path:      w.path,
	}
	setSpan(&node, d)
	node.DocStartByte, node.DocEndByte = docSpanBefore(d, w.lang, w.src, isSCSSComment)
	w.nodes = append(w.nodes, node)
	w.link(parent, idx)
}

// paramsOf returns a mixin or function's parameter list as written, or "" for
// the parameterless form. Keeping the source text means default values and rest
// arguments survive into the signature without the extractor having to model
// Sass expression syntax.
func (w *scssWalk) paramsOf(n *tsg.Node) string {
	params := childByType(n, "parameters", w.lang)
	if params == nil {
		return ""
	}
	return normaliseSpace(params.Text(w.src))
}

// finish resolves the held `@include` sites against the mixins the file
// defines. An include naming a mixin from another file resolves to nothing and
// is dropped rather than guessed at: the topology call graph is intra-file, and
// a fabricated edge is worse than a missing one.
func (w *scssWalk) finish() ([]topology.Node, []topology.Edge) {
	counts := callableNameCounts(w.nodes)
	for _, p := range w.pending {
		to, ok := w.callables[p.name]
		if !ok || p.from == to {
			continue
		}
		w.edges = append(w.edges, heuristicCallEdge(p.from, to, w.nodes, counts))
	}
	return w.nodes, w.edges
}

// leadingSassVariable returns the `$name` a declaration assigns, or "" when the
// declaration is an ordinary property. The `$` prefix is the whole test, which
// mirrors how leadingCustomProperty keys off `--`.
//
// The value is deliberately not parsed. A real variable line often carries
// `!default`, which the grammar mis-parses into a sibling ERROR node — but the
// declaration itself, and so the name, survives, and that graceful degradation
// is only available to a rule this blunt.
func leadingSassVariable(decl string) string {
	decl = strings.TrimSpace(decl)
	if !strings.HasPrefix(decl, "$") {
		return ""
	}
	end := strings.IndexAny(decl, ": \t\r\n")
	if end <= 1 {
		return ""
	}
	return decl[:end]
}

// isSCSSComment covers both comment syntaxes Sass accepts. The grammar names
// the `//` form js_comment, which is easy to miss and would silently cost every
// doc span in a codebase that prefers it — which most Sass codebases do, since
// `//` comments are stripped from the compiled output and `/* */` are not.
func isSCSSComment(typ string) bool {
	return typ == "comment" || typ == "js_comment" || typ == "block_comment"
}
