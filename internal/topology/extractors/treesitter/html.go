package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/golimpio/plumb/internal/topology"
)

// HTMLExtractor extracts navigable document landmarks from HTML using the
// gotreesitter HTML grammar. HTML has no functions or types, so the extractor
// surfaces the elements that actually serve as navigation targets:
//
//   - headings (h1–h6)            → KindSection — the document outline
//   - elements with an `id`       → KindConstant — addressable anchors (#id targets)
//   - <script src> / <link href> → KindImport — external resource references
//   - custom elements (a hyphen
//     in the tag name, e.g.
//     <my-widget>)                → KindClass — web-component instances
//
// Containment follows the DOM: an interesting element nested inside another
// interesting element gets a certain (1.0) contains edge, so a heading inside a
// `<section id="…">` is recorded under that section. Plain, uninteresting
// elements are transparent — they pass their parent through to their children.
//
// Concurrency: stateless after construction and safe for concurrent use; a
// fresh parser is created per Extract call because gotreesitter parsers are not
// safe for concurrent reuse.
type HTMLExtractor struct {
	lang *tsg.Language
}

// NewHTML returns a tree-sitter-backed HTML extractor.
func NewHTML() *HTMLExtractor {
	return &HTMLExtractor{lang: grammars.HtmlLanguage()}
}

func (e *HTMLExtractor) Language() string     { return "html" }
func (e *HTMLExtractor) Extensions() []string { return []string{".html", ".htm"} }

// Extract parses src and returns the landmark nodes described on HTMLExtractor,
// with DOM-nesting containment edges between them. Returns (nil, nil, nil) when
// src cannot be parsed.
func (e *HTMLExtractor) Extract(_ context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	tree, err := tsg.NewParser(e.lang).Parse(src)
	if err != nil || tree == nil {
		return nil, nil, nil
	}
	w := &htmlWalk{lang: e.lang, src: src, path: relPath}
	w.walk(tree.RootNode(), -1)
	return w.nodes, w.edges, nil
}

type htmlWalk struct {
	lang  *tsg.Language
	src   []byte
	path  string
	nodes []topology.Node
	edges []topology.Edge
}

// walk descends the DOM. parent is the node index of the nearest enclosing
// *interesting* element (-1 at document scope). An element that classifies as
// interesting emits a node and becomes the containment parent for its
// descendants; an uninteresting element passes parent through unchanged.
func (w *htmlWalk) walk(n *tsg.Node, parent int64) {
	for _, c := range n.Children() {
		next := parent
		switch c.Type(w.lang) {
		case "element", "script_element", "style_element":
			if idx := w.classify(c, parent); idx >= 0 {
				next = idx
			}
		}
		w.walk(c, next)
	}
}

// classify emits a node for el when it is a navigation landmark and returns its
// index, or -1 when el is not interesting. Precedence: heading, then external
// reference (script/link), then id anchor, then custom element — so a
// `<script src>` is an import (not its id) and a `<my-widget id>` is an anchor.
func (w *htmlWalk) classify(el *tsg.Node, parent int64) int64 {
	tag := w.tagName(el)
	if tag == "" {
		return -1
	}
	if isHeading(tag) {
		if name := w.headingName(el); name != "" {
			return w.emit(el, parent, topology.KindSection, name)
		}
		return -1
	}
	if ref := w.externalRef(el, tag); ref != "" {
		return w.emit(el, parent, topology.KindImport, ref)
	}
	if id := w.attr(el, "id"); id != "" {
		return w.emit(el, parent, topology.KindConstant, id)
	}
	if strings.Contains(tag, "-") {
		return w.emit(el, parent, topology.KindClass, tag)
	}
	return -1
}

// externalRef returns the external resource a <script>/<link> element points
// at (its src/href), or "" for any other element or an inline script/style.
func (w *htmlWalk) externalRef(el *tsg.Node, tag string) string {
	switch tag {
	case "script":
		return w.attr(el, "src")
	case "link":
		return w.attr(el, "href")
	}
	return ""
}

func (w *htmlWalk) emit(el *tsg.Node, parent int64, kind topology.NodeKind, name string) int64 {
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		StartLine: line(el.StartPoint()),
		EndLine:   line(el.EndPoint()),
		Language:  "html",
		Path:      w.path,
	})
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

// startTag returns an element's opening tag, whether a normal `start_tag` or a
// `self_closing_tag` (e.g. <img/>), or nil when the element has neither.
func (w *htmlWalk) startTag(el *tsg.Node) *tsg.Node {
	if st := childByType(el, "start_tag", w.lang); st != nil {
		return st
	}
	return childByType(el, "self_closing_tag", w.lang)
}

// tagName returns the element's lower-cased tag name, or "".
func (w *htmlWalk) tagName(el *tsg.Node) string {
	st := w.startTag(el)
	if st == nil {
		return ""
	}
	if tn := childByType(st, "tag_name", w.lang); tn != nil {
		return strings.ToLower(strings.TrimSpace(tn.Text(w.src)))
	}
	return ""
}

// attr returns the value of the named attribute on el (case-insensitive name
// match), or "" when absent. Quoted and unquoted values are both handled.
func (w *htmlWalk) attr(el *tsg.Node, name string) string {
	st := w.startTag(el)
	if st == nil {
		return ""
	}
	for _, c := range st.Children() {
		if c.Type(w.lang) != "attribute" {
			continue
		}
		an := childByType(c, "attribute_name", w.lang)
		if an == nil || !strings.EqualFold(strings.TrimSpace(an.Text(w.src)), name) {
			continue
		}
		return w.attrValue(c)
	}
	return ""
}

// attrValue extracts the text of an attribute node's value, unwrapping a
// quoted_attribute_value when present.
func (w *htmlWalk) attrValue(attr *tsg.Node) string {
	if q := childByType(attr, "quoted_attribute_value", w.lang); q != nil {
		if v := childByType(q, "attribute_value", w.lang); v != nil {
			return strings.TrimSpace(v.Text(w.src))
		}
		return ""
	}
	if v := childByType(attr, "attribute_value", w.lang); v != nil {
		return strings.TrimSpace(v.Text(w.src))
	}
	return ""
}

// headingName returns the collapsed visible text of a heading element, or ""
// for an empty heading (which is then skipped, mirroring the Markdown extractor).
func (w *htmlWalk) headingName(el *tsg.Node) string {
	return strings.Join(strings.Fields(w.innerText(el)), " ")
}

// innerText concatenates the text of every `text` descendant of n, separated by
// spaces — enough to name a heading without reproducing inline markup.
func (w *htmlWalk) innerText(n *tsg.Node) string {
	var sb strings.Builder
	var rec func(*tsg.Node)
	rec = func(node *tsg.Node) {
		for _, c := range node.Children() {
			if c.Type(w.lang) == "text" {
				sb.WriteString(c.Text(w.src))
				sb.WriteByte(' ')
			} else {
				rec(c)
			}
		}
	}
	rec(n)
	return sb.String()
}

// isHeading reports whether tag is one of h1–h6.
func isHeading(tag string) bool {
	return len(tag) == 2 && tag[0] == 'h' && tag[1] >= '1' && tag[1] <= '6'
}
