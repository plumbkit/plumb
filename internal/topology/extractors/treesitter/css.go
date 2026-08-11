package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// CSSExtractor extracts CSS landmarks using the gotreesitter CSS grammar.
//
// Concurrency: stateless after construction and safe for concurrent use; a
// fresh parser is created per Extract call because gotreesitter parsers are not
// safe for concurrent reuse.
type CSSExtractor struct {
	lang lazyGrammar
}

// NewCSS returns a tree-sitter-backed CSS extractor.
func NewCSS() *CSSExtractor {
	return &CSSExtractor{lang: lazyGrammar{load: grammars.CssLanguage}}
}

func (e *CSSExtractor) Language() string     { return "css" }
func (e *CSSExtractor) Extensions() []string { return []string{".css"} }

// Extract parses src and returns the landmarks someone actually navigates a
// stylesheet by: rule sets named by their selector, `@media`/`@supports` blocks
// as sections containing the rules nested inside them, `@keyframes` by name,
// custom properties as constants, and `@import` as imports.
//
// Ordinary declarations (`color: red`) are deliberately NOT emitted. A
// stylesheet has one or two orders of magnitude more declarations than
// selectors, and they are the same handful of property names repeated — so
// emitting them buries the selectors that carry the structure, which is the
// per-node explosion HTMLExtractor's doc comment records (a node per tag AND
// attribute gave ~1220 nodes for one page against ~54 useful landmarks).
//
// Custom properties are the one exception, and they earn it: `--brand-colour`
// is a design token with a name someone searches for by name, there are few of
// them, and they are usually declared once in `:root`.
func (e *CSSExtractor) Extract(ctx context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	lang := e.lang.get()
	return extractWith(ctx, lang, src, func(root *tsg.Node) ([]topology.Node, []topology.Edge) {
		w := &cssWalk{lang: lang, src: src, path: relPath, langName: "css"}
		w.walk(root, -1, "")
		return w.nodes, w.edges
	})
}

// cssWalk is shared with the SCSS extractor, which is a superset of CSS:
// langName keeps each one's nodes stamped with its own language.
type cssWalk struct {
	lang     *tsg.Language
	src      []byte
	path     string
	langName string
	nodes    []topology.Node
	edges    []topology.Edge
}

// walk descends one level of the stylesheet, carrying the enclosing section (an
// at-rule block) so nesting becomes containment.
func (w *cssWalk) walk(n *tsg.Node, parent int64, prefix string) {
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "import_statement":
			w.addImport(c)
		case "rule_set":
			w.addRuleSet(c, parent, prefix)
		case "media_statement", "supports_statement":
			w.addSection(c, parent, prefix)
		case "keyframes_statement":
			w.addKeyframes(c, parent)
		}
	}
}

func (w *cssWalk) addImport(n *tsg.Node) {
	name := w.stringArg(n)
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

// addRuleSet emits one node for the whole rule, named by its selector list.
//
// A grouped selector (`.btn, .btn-primary { … }`) is one rule with one body, so
// it is one node rather than one per selector: splitting it would emit several
// nodes sharing an identical span, which reads as duplicate symbols to every
// consumer. The full list stays in the name, and FTS tokenisation still matches
// a search for either half.
func (w *cssWalk) addRuleSet(n *tsg.Node, parent int64, prefix string) {
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
	node.DocStartByte, node.DocEndByte = docSpanBefore(n, w.lang, w.src, isCComment)
	w.nodes = append(w.nodes, node)
	w.link(parent, idx)
	if block := childByType(n, "block", w.lang); block != nil {
		w.addCustomProperties(block, idx, node.Qualified)
	}
}

// addSection emits an `@media` / `@supports` block and descends into it, so a
// rule that only applies under a query is reachable through that query rather
// than floating at the top level indistinguishable from an unconditional one.
func (w *cssWalk) addSection(n *tsg.Node, parent int64, prefix string) {
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
		w.walk(block, idx, node.Qualified)
	}
}

func (w *cssWalk) addKeyframes(n *tsg.Node, parent int64) {
	id := childByType(n, "keyframes_name", w.lang)
	if id == nil {
		return
	}
	name := id.Text(w.src)
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      topology.KindType,
		Name:      name,
		Qualified: name,
		Signature: "@keyframes " + name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  w.langName,
		Path:      w.path,
	}
	setSpan(&node, n)
	w.nodes = append(w.nodes, node)
	w.link(parent, idx)
}

// addCustomProperties emits `--name: value` declarations, and only those.
func (w *cssWalk) addCustomProperties(block *tsg.Node, parent int64, prefix string) {
	for _, d := range block.Children() {
		if d.Type(w.lang) != "declaration" {
			continue
		}
		name := leadingCustomProperty(d.Text(w.src))
		if name == "" {
			continue
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
		w.nodes = append(w.nodes, node)
		w.link(parent, idx)
	}
}

// headOf returns an at-rule's text up to its block, i.e. `@media (min-width:
// 600px)` without the body.
func (w *cssWalk) headOf(n *tsg.Node) string {
	block := childByType(n, "block", w.lang)
	end := n.EndByte()
	if block != nil {
		end = block.StartByte()
	}
	lo, hi := clampU32(n.StartByte()), clampU32(end)
	if lo >= hi || hi > len(w.src) {
		return ""
	}
	return string(w.src[lo:hi])
}

func (w *cssWalk) stringArg(n *tsg.Node) string {
	if sv := childByType(n, "string_value", w.lang); sv != nil {
		if content := childByType(sv, "string_content", w.lang); content != nil {
			return content.Text(w.src)
		}
		return strings.Trim(sv.Text(w.src), `"'`)
	}
	// `@import url(base.css);` and bare-word forms.
	for _, c := range n.Children() {
		if c.IsNamed() && c.Type(w.lang) != "at_keyword" {
			return strings.Trim(normaliseSpace(c.Text(w.src)), `"'`)
		}
	}
	return ""
}

func (w *cssWalk) link(parent, child int64) {
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

// leadingCustomProperty returns the custom-property name of a declaration, or
// "" for an ordinary one. CSS marks custom properties with a `--` prefix, which
// is the whole test.
func leadingCustomProperty(decl string) string {
	decl = strings.TrimSpace(decl)
	if !strings.HasPrefix(decl, "--") {
		return ""
	}
	end := strings.IndexAny(decl, ": \t\r\n")
	if end <= 2 {
		return ""
	}
	return decl[:end]
}

// qualifyCSS joins an enclosing at-rule to a nested name with " > ", so a rule
// inside a media query reads as `@media (min-width: 600px) > .btn`.
func qualifyCSS(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + " > " + name
}

// normaliseSpace collapses the whitespace a multi-line selector list or feature
// query carries, so the name is one stable line regardless of formatting.
func normaliseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
