package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// XMLExtractor extracts navigable landmarks from XML documents using the
// gotreesitter XML grammar.
//
// XML has no functions, types or calls, so the question is not how to model a
// language but where to stop: a Maven pom or an Android manifest has hundreds of
// elements, and a node per element would bury the handful worth navigating to.
// That is the explosion HTMLExtractor's doc comment records (a node per tag AND
// attribute gave ~1220 nodes for one page against ~54 useful landmarks), and
// this extractor follows the answer HTML arrived at — classify, do not
// enumerate. An element earns a node when it is one of:
//
//   - an import — `xs:import`/`xs:include`/`xsl:import`/`xsl:include` by their
//     `schemaLocation`/`href`, plus an `<?xml-stylesheet href="…"?>`
//     instruction → KindImport
//   - identified — carrying a `name`, `id`, `key`, `match` or `ref` attribute,
//     which is what makes `<bean id="userService">`, `<xs:element name="Order">`,
//     `<xsl:template match="/">` and `<activity android:name="…">` addressable
//     → KindConstant
//   - structural — the root element and its direct children, which give a
//     document without identifying attributes (a pom is the common case) the
//     outline someone actually navigates → KindSection
//
// Everything else is transparent: it emits nothing and passes its parent
// through to its children, so an identified element deep inside layers of
// wrapper tags is still recorded under the nearest landmark above it rather
// than being lost or forced to invent intermediate nodes.
//
// Concurrency: stateless after construction and safe for concurrent use; a
// fresh parser is created per Extract call because gotreesitter parsers are not
// safe for concurrent reuse.
type XMLExtractor struct {
	lang lazyGrammar
}

// NewXML returns a tree-sitter-backed XML extractor.
func NewXML() *XMLExtractor {
	return &XMLExtractor{lang: lazyGrammar{load: grammars.XmlLanguage}}
}

func (e *XMLExtractor) Language() string     { return "xml" }
func (e *XMLExtractor) Extensions() []string { return []string{".xml", ".xsd", ".xsl"} }

// Extract parses src and returns the landmarks described on XMLExtractor, with
// element nesting as containment.
func (e *XMLExtractor) Extract(ctx context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	lang := e.lang.get()
	return extractWith(ctx, lang, src, func(root *tsg.Node) ([]topology.Node, []topology.Edge) {
		// The XML grammar returns a nil root for empty input, which no other
		// grammar in this package does — JSON, HTML and TOML all hand back a
		// real (if childless) root — so extractWith has no reason to guard it
		// and the guard has to live here. Without it an empty .xml file panics
		// the indexer.
		if root == nil {
			return nil, nil
		}
		w := &xmlWalk{lang: lang, src: src, path: relPath}
		w.walk(root, -1, "", 0)
		return w.nodes, w.edges
	})
}

// xmlMaxRepeatedSiblings bounds how many same-tag children of one element are
// walked.
//
// This is the JSON extractor's jsonMaxArrayElements rule translated: a long run
// of identical tags is a data array, not structure, and it is the same shape
// repeated. Without a bound, one file in a real 1,201-file sweep produced
// 14,188 nodes on its own (`x86-intel.xml`, a list of CPU intrinsics), which
// buries every document around it in ranked search.
//
// 500 is measured rather than chosen. Sweeping 1,201 real files (26.8 MB) at
// several caps, against a ground truth of every `name=`/`id=` attribute:
//
//	cap    total nodes   densest file   identity recall
//	 20         47,780            763             38.5%
//	 50         66,323          1,613             50.0%
//	100         81,506          2,696             58.9%
//	200         96,612          2,947             68.9%
//	500        109,449          2,993             78.4%
//	none      132,025         14,188             89.9%
//
// The worst-case file barely moves between 200 and 500 (2,947 → 2,993) while
// recall gains nearly ten points, so 500 buys back most of the coverage a cap
// costs without letting any single document dominate. A heterogeneous document
// — a pom, a manifest, a schema — never comes near it. Siblings past the cap are
// not descended into; nothing already emitted is dropped.
const xmlMaxRepeatedSiblings = 500

type xmlWalk struct {
	lang  *tsg.Language
	src   []byte
	path  string
	nodes []topology.Node
	edges []topology.Edge
}

// walk descends the document. parent is the node index of the nearest enclosing
// landmark (-1 at document scope), prefix its XPath-style qualified name, and
// depth counts ELEMENT nesting rather than tree nesting — the grammar puts a
// `content` node between every element and its children, so counting tree
// levels would make "the root's direct children" mean the wrong thing.
func (w *xmlWalk) walk(n *tsg.Node, parent int64, prefix string, depth int) {
	// Counted per container, so a long run of <intrinsic> under one parent is
	// bounded while the same tag used once under many parents is untouched.
	seen := map[string]int{}
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "element":
			next, nextPrefix := parent, prefix
			tag := w.tagName(c)
			seen[tag]++
			if seen[tag] > xmlMaxRepeatedSiblings {
				continue
			}
			if idx := w.classify(c, parent, prefix, depth); idx >= 0 {
				next = idx
				nextPrefix = w.nodes[idx].Qualified
			} else if tag != "" {
				// Transparent, but it still contributes to the path so a
				// qualified name stays a usable XPath.
				nextPrefix = joinXPath(prefix, tag)
			}
			w.walk(c, next, nextPrefix, depth+1)
		case "prolog":
			w.addStylesheetPI(c)
			w.walk(c, parent, prefix, depth)
		default:
			// `content`, `STag`, `CDSect` and friends carry no landmark of
			// their own but do carry the elements nested inside them.
			w.walk(c, parent, prefix, depth)
		}
	}
}

// classify decides whether an element is a landmark and emits it if so,
// returning its node index or -1. The order is deliberate: an import is an
// import even when it also carries a name, and an identified element is worth
// more than the fact that it sits near the top of the document.
func (w *xmlWalk) classify(el *tsg.Node, parent int64, prefix string, depth int) int64 {
	tag := w.tagName(el)
	if tag == "" {
		return -1
	}
	if ref := w.importRef(el, tag); ref != "" {
		return w.emit(el, parent, prefix, topology.KindImport, ref, tag)
	}
	if name, attr := w.identifier(el); name != "" {
		return w.emit(el, parent, prefix, topology.KindConstant, name, tag+" "+attr)
	}
	if depth <= 1 {
		return w.emit(el, parent, prefix, topology.KindSection, tag, tag)
	}
	return -1
}

func (w *xmlWalk) emit(el *tsg.Node, parent int64, prefix string, kind topology.NodeKind, name, sig string) int64 {
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: joinXPath(prefix, name),
		Signature: sig,
		StartLine: line(el.StartPoint()),
		EndLine:   line(el.EndPoint()),
		Language:  "xml",
		Path:      w.path,
	}
	setSpan(&node, el)
	node.DocStartByte, node.DocEndByte = w.docSpan(el)
	w.nodes = append(w.nodes, node)
	w.link(parent, idx)
	return idx
}

// addStylesheetPI records `<?xml-stylesheet href="a.xsl"?>`, the one import an
// XML document can carry outside its root element.
func (w *xmlWalk) addStylesheetPI(prolog *tsg.Node) {
	for _, c := range prolog.Children() {
		if c.Type(w.lang) != "StyleSheetPI" {
			continue
		}
		for _, att := range c.Children() {
			if att.Type(w.lang) != "PseudoAtt" {
				continue
			}
			nameNode := childByType(att, "Name", w.lang)
			valNode := childByType(att, "PseudoAttValue", w.lang)
			if nameNode == nil || valNode == nil || nameNode.Text(w.src) != "href" {
				continue
			}
			href := trimAttr(valNode.Text(w.src))
			if href == "" {
				continue
			}
			node := topology.Node{
				Kind:      topology.KindImport,
				Name:      href,
				Qualified: href,
				Signature: "xml-stylesheet",
				StartLine: line(c.StartPoint()),
				EndLine:   line(c.EndPoint()),
				Language:  "xml",
				Path:      w.path,
			}
			setSpan(&node, c)
			w.nodes = append(w.nodes, node)
		}
	}
}

// tagName returns an element's tag as written, prefix included, from whichever
// of the two opening forms the grammar produced.
func (w *xmlWalk) tagName(el *tsg.Node) string {
	for _, typ := range []string{"STag", "EmptyElemTag"} {
		if tag := childByType(el, typ, w.lang); tag != nil {
			if n := childByType(tag, "Name", w.lang); n != nil {
				return n.Text(w.src)
			}
		}
	}
	return ""
}

// identifier returns the value of the first identifying attribute an element
// carries, and which attribute it was.
//
// The order is by how strongly the attribute names the element: `name` and `id`
// are identity, `key` and `match` are identity in the schema and stylesheet
// dialects, and `ref` is a pointer at something named elsewhere and so the
// weakest. A namespaced spelling (`android:name`, `xml:id`) counts as the same
// attribute, which is what makes an Android manifest legible.
func (w *xmlWalk) identifier(el *tsg.Node) (value, attr string) {
	for _, want := range []string{"name", "id", "key", "match", "ref"} {
		if v := w.attr(el, want); v != "" {
			return v, want
		}
	}
	return "", ""
}

// attr returns an element's attribute value, matching on the local name so a
// namespace prefix does not hide it. Returns "" when absent or empty.
func (w *xmlWalk) attr(el *tsg.Node, want string) string {
	for _, typ := range []string{"STag", "EmptyElemTag"} {
		tag := childByType(el, typ, w.lang)
		if tag == nil {
			continue
		}
		for _, a := range tag.Children() {
			if a.Type(w.lang) != "Attribute" {
				continue
			}
			nameNode := childByType(a, "Name", w.lang)
			valNode := childByType(a, "AttValue", w.lang)
			if nameNode == nil || valNode == nil {
				continue
			}
			if localName(nameNode.Text(w.src)) != want {
				continue
			}
			if v := trimAttr(valNode.Text(w.src)); v != "" {
				return v
			}
		}
	}
	return ""
}

// importRef returns the location an import-shaped element points at, or "".
// Both the schema and stylesheet dialects spell this as an `import`/`include`
// element, differing only in the attribute that carries the target.
func (w *xmlWalk) importRef(el *tsg.Node, tag string) string {
	switch localName(tag) {
	case "import", "include", "redefine", "override":
	default:
		return ""
	}
	for _, a := range []string{"schemaLocation", "href"} {
		if v := w.attr(el, a); v != "" {
			return v
		}
	}
	return ""
}

func (w *xmlWalk) link(parent, child int64) {
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

// localName strips a namespace prefix: `xs:element` → `element`.
func localName(tag string) string {
	if i := strings.IndexByte(tag, ':'); i >= 0 {
		return tag[i+1:]
	}
	return tag
}

// trimAttr removes the quotes the grammar keeps as part of an attribute value.
func trimAttr(v string) string {
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(v), `"'`))
}

// joinXPath builds the `project/dependencies/dependency` qualified names that
// are how anyone refers to a position in an XML document.
func joinXPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "/" + name
}

// docSpan returns the comment block directly above an element.
//
// The shared docSpanBefore cannot be used as-is here, and it is worth being
// precise about why rather than quietly forking it. XML preserves inter-element
// whitespace as CharData nodes, so in any indented document an element's
// PrevSibling is a run of whitespace rather than the comment above it — and
// docSpanBefore looks only at the immediate PrevSibling, so it would return
// nothing for every element in every well-formatted file. This applies the same
// rule, including the same flushness check, with blank CharData skipped.
//
// The root element gets one further accommodation: a document's leading comment
// is its doc comment, but the XML spec files everything before the root under
// `prolog`, so the comment is not the root's sibling at all.
func (w *xmlWalk) docSpan(el *tsg.Node) (start, end int) {
	sibs := siblingsOf(el)
	prev := w.skipBlank(sibs, nodeIndexIn(sibs, el)-1)
	if prev < 0 {
		return 0, 0
	}
	if w.lang != nil && sibs[prev].Type(w.lang) == "prolog" {
		return w.prologDocSpan(sibs[prev], el)
	}
	if !w.isComment(sibs[prev]) || !commentFlushBefore(w.src, sibs[prev], el.StartByte()) {
		return 0, 0
	}
	last, first := sibs[prev], sibs[prev]
	for j := w.skipBlank(sibs, prev-1); j >= 0; j = w.skipBlank(sibs, j-1) {
		if !w.isComment(sibs[j]) || !commentFlushBefore(w.src, sibs[j], first.StartByte()) {
			break
		}
		first = sibs[j]
	}
	return clampU32(first.StartByte()), clampU32(last.EndByte())
}

// prologDocSpan takes the last comment in the prolog when it sits directly
// above the root element.
func (w *xmlWalk) prologDocSpan(prolog, el *tsg.Node) (start, end int) {
	kids := prolog.Children()
	for i := len(kids) - 1; i >= 0; i-- {
		if !w.isComment(kids[i]) {
			continue
		}
		if !commentFlushBefore(w.src, kids[i], el.StartByte()) {
			return 0, 0
		}
		return clampU32(kids[i].StartByte()), clampU32(kids[i].EndByte())
	}
	return 0, 0
}

// skipBlank walks backwards past whitespace-only CharData, which carries no
// meaning but does sit between every pair of nodes in an indented document.
func (w *xmlWalk) skipBlank(sibs []*tsg.Node, i int) int {
	for i >= 0 && sibs[i].Type(w.lang) == "CharData" && strings.TrimSpace(sibs[i].Text(w.src)) == "" {
		i--
	}
	return i
}

// isComment names the grammar's comment node, which follows the XML spec's
// production name rather than the lowercase convention most grammars use.
func (w *xmlWalk) isComment(n *tsg.Node) bool { return n.Type(w.lang) == "Comment" }
