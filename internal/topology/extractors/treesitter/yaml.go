package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// YAMLExtractor extracts YAML configuration symbols using the gotreesitter YAML
// grammar. Every mapping key becomes a field; nesting becomes containment.
//
// Concurrency: stateless after construction and safe for concurrent use; a
// fresh parser is created per Extract call because gotreesitter parsers are not
// safe for concurrent reuse.
type YAMLExtractor struct {
	lang lazyGrammar
}

// NewYAML returns a tree-sitter-backed YAML extractor.
func NewYAML() *YAMLExtractor {
	return &YAMLExtractor{lang: lazyGrammar{load: grammars.YamlLanguage}}
}

func (e *YAMLExtractor) Language() string     { return "yaml" }
func (e *YAMLExtractor) Extensions() []string { return []string{".yaml", ".yml"} }

// Extract parses src and returns each block-mapping key as a field, linked to
// its enclosing key by a certain (1.0) containment edge so the config tree is
// navigable (e.g. services → web → image for docker-compose). Each field's
// Qualified is the dotted path of its enclosing keys (services.web.image),
// matching the SQL/TOML convention. Keys reached through a block sequence
// (lists of objects) are attached to the nearest enclosing key. Returns
// (nil, nil, nil) when src cannot be parsed.
func (e *YAMLExtractor) Extract(_ context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	tree, err := tsg.NewParser(e.lang.get()).Parse(src)
	if err != nil || tree == nil {
		return nil, nil, nil
	}
	defer tree.Release()
	w := &yamlWalk{lang: e.lang.get(), src: src, path: relPath}
	w.walkNode(tree.RootNode(), -1, "")
	return w.nodes, w.edges, nil
}

type yamlWalk struct {
	lang  *tsg.Language
	src   []byte
	path  string
	nodes []topology.Node
	edges []topology.Edge
}

// walkNode descends the tree, emitting a field per mapping key. Non-pair nodes
// (documents, sequences, block/flow wrappers) are transparent — children are
// visited with the same parent.
func (w *yamlWalk) walkNode(n *tsg.Node, parent int64, prefix string) {
	if n.Type(w.lang) == "block_mapping_pair" {
		w.handlePair(n, parent, prefix)
		return
	}
	for _, c := range n.Children() {
		w.walkNode(c, parent, prefix)
	}
}

func (w *yamlWalk) handlePair(n *tsg.Node, parent int64, prefix string) {
	key := w.keyText(n)
	if key == "" {
		for _, c := range n.Children() {
			w.walkNode(c, parent, prefix)
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
		Language:  "yaml",
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
	if val := w.valueNode(n); val != nil {
		w.walkNode(val, idx, qualified)
	}
}

// keyText returns the mapping key's scalar text with quotes trimmed.
func (w *yamlWalk) keyText(pair *tsg.Node) string {
	k := pair.ChildByFieldName("key", w.lang)
	if k == nil {
		k = firstNamedChild(pair)
	}
	if k == nil {
		return ""
	}
	return strings.Trim(strings.TrimSpace(k.Text(w.src)), `"'`)
}

// valueNode returns the pair's value (a scalar or a nested block), falling back
// to the last named child when the grammar does not expose the value field.
func (w *yamlWalk) valueNode(pair *tsg.Node) *tsg.Node {
	if v := pair.ChildByFieldName("value", w.lang); v != nil {
		return v
	}
	var last *tsg.Node
	for _, c := range pair.Children() {
		if c.IsNamed() {
			last = c
		}
	}
	if last == pair.ChildByFieldName("key", w.lang) {
		return nil
	}
	return last
}
