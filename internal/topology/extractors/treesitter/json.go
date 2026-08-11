package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// jsonMaxArrayElements bounds how many elements of a JSON array are walked
// for nested keys. JSON has no syntax for naming an array element, so a
// document with one big array of near-identical records (a page of API
// results, a log of events) would otherwise emit one field per key per
// element — tens of thousands of near-duplicate nodes carrying no more
// search value than the first handful, since the schema repeats across
// entries. Capping the walk keeps a small config array (docker-compose-style
// port/volume lists, a package.json "files" list) fully indexed while
// stopping a pathological data file from blowing out the node count; the
// elements past the cap are simply not descended into, not dropped from the
// source.
const jsonMaxArrayElements = 20

// JSONExtractor extracts JSON/JSONC configuration symbols using the
// gotreesitter JSON grammar. Every object key becomes a field, named by its
// dotted path from the document root (e.g. scripts.build), and nesting
// becomes containment — the same convention TOMLExtractor and YAMLExtractor
// use, so a search for a key behaves the same across config formats.
//
// Concurrency: stateless after construction and safe for concurrent use; a
// fresh parser is created per Extract call because gotreesitter parsers are
// not safe for concurrent reuse.
type JSONExtractor struct {
	lang lazyGrammar
}

// NewJSON returns a tree-sitter-backed JSON extractor.
func NewJSON() *JSONExtractor {
	return &JSONExtractor{lang: lazyGrammar{load: grammars.JsonLanguage}}
}

func (e *JSONExtractor) Language() string     { return "json" }
func (e *JSONExtractor) Extensions() []string { return []string{".json", ".jsonc"} }

// Extract parses src and returns each object key as a field, linked to its
// enclosing key by a certain (1.0) containment edge so the document tree is
// navigable (e.g. scripts → build in a package.json). Each field's Qualified
// is the dotted path of its enclosing keys. An array is walked transparently
// — it never becomes a node itself, and an element's keys attach to whatever
// key held the array, exactly as a YAML block sequence is handled — up to
// jsonMaxArrayElements per array (see its doc comment for why). Returns
// (nil, nil, nil) when src cannot be parsed.
func (e *JSONExtractor) Extract(ctx context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	lang := e.lang.get()
	return extractWith(ctx, lang, src, func(root *tsg.Node) ([]topology.Node, []topology.Edge) {
		w := &jsonWalk{lang: lang, src: src, path: relPath}
		for _, c := range root.Children() {
			if c.IsNamed() {
				w.walkValue(c, -1, "")
			}
		}
		return w.nodes, w.edges
	})
}

type jsonWalk struct {
	lang  *tsg.Language
	src   []byte
	path  string
	nodes []topology.Node
	edges []topology.Edge
}

// walkValue descends into one JSON value. An object contributes a field per
// pair (handlePair recurses into each pair's own value); an array is
// transparent — its elements are walked with the SAME parent and qualified
// prefix as the array itself, since JSON has no syntax for naming an
// individual element, capped at jsonMaxArrayElements. A scalar (string,
// number, true, false, null) carries no keys and ends the descent.
func (w *jsonWalk) walkValue(n *tsg.Node, parent int64, prefix string) {
	switch n.Type(w.lang) {
	case "object":
		for _, c := range n.Children() {
			if c.Type(w.lang) == "pair" {
				w.handlePair(c, parent, prefix)
			}
		}
	case "array":
		count := 0
		for _, c := range n.Children() {
			if !c.IsNamed() {
				continue // "[", ",", "]" punctuation
			}
			if count >= jsonMaxArrayElements {
				break
			}
			count++
			w.walkValue(c, parent, prefix)
		}
	}
}

// handlePair records a "key": value pair as a field, then walks its value
// under the field's own index so nested keys are contained by it.
func (w *jsonWalk) handlePair(n *tsg.Node, parent int64, prefix string) {
	key := w.keyText(n)
	if key == "" {
		// A key tree-sitter's error recovery could not resolve. Still descend
		// into the value in case it holds otherwise-valid nested keys — just
		// without a field of its own for them to attach to, matching how
		// YAMLExtractor treats an unreadable mapping key.
		if val := n.ChildByFieldName("value", w.lang); val != nil {
			w.walkValue(val, parent, prefix)
		}
		return
	}
	qualified := key
	if prefix != "" {
		qualified = prefix + "." + key
	}
	idx := int64(len(w.nodes))
	node := topology.Node{
		Kind:      topology.KindField,
		Name:      key,
		Qualified: qualified,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "json",
		Path:      w.path,
	}
	setSpan(&node, n)
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
	if val := n.ChildByFieldName("value", w.lang); val != nil {
		w.walkValue(val, idx, qualified)
	}
}

// keyText returns a pair's key with its surrounding quotes stripped. Unlike
// TOML/YAML, JSON has only the one key form (a double-quoted string) — no
// bare or dotted key to also handle.
func (w *jsonWalk) keyText(pair *tsg.Node) string {
	k := pair.ChildByFieldName("key", w.lang)
	if k == nil {
		return ""
	}
	return strings.Trim(k.Text(w.src), `"`)
}
