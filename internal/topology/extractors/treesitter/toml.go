package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// TOMLExtractor extracts TOML configuration symbols using the gotreesitter TOML
// grammar.
//
// Concurrency: stateless after construction and safe for concurrent use; a
// fresh parser is created per Extract call because gotreesitter parsers are not
// safe for concurrent reuse.
type TOMLExtractor struct {
	lang lazyGrammar
}

// NewTOML returns a tree-sitter-backed TOML extractor.
func NewTOML() *TOMLExtractor {
	return &TOMLExtractor{lang: lazyGrammar{load: grammars.TomlLanguage}}
}

func (e *TOMLExtractor) Language() string     { return "toml" }
func (e *TOMLExtractor) Extensions() []string { return []string{".toml"} }

// Extract parses src and returns each `[table]` / `[[array.table]]` header as a
// type (named by its dotted key), each top-level `key = value` pair and each
// pair inside a table as a field, with table→field links as certain (1.0)
// containment edges. Returns (nil, nil, nil) when src cannot be parsed.
func (e *TOMLExtractor) Extract(_ context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	tree, err := tsg.NewParser(e.lang.get()).Parse(src)
	if err != nil || tree == nil {
		return nil, nil, nil
	}
	defer tree.Release()
	w := &tomlWalk{lang: e.lang.get(), src: src, path: relPath}
	w.walk(tree.RootNode())
	return w.nodes, w.edges, nil
}

type tomlWalk struct {
	lang  *tsg.Language
	src   []byte
	path  string
	nodes []topology.Node
	edges []topology.Edge
}

func (w *tomlWalk) walk(root *tsg.Node) {
	for _, n := range root.Children() {
		switch n.Type(w.lang) {
		case "pair":
			w.addField(n, -1, "")
		case "table", "table_array_element":
			w.addTable(n)
		}
	}
}

func (w *tomlWalk) addTable(n *tsg.Node) {
	name := w.firstKey(n)
	if name == "" {
		return
	}
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      topology.KindType,
		Name:      name,
		Qualified: name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "toml",
		Path:      w.path,
	})
	for _, c := range n.Children() {
		if c.Type(w.lang) == "pair" {
			w.addField(c, idx, name)
		}
	}
}

// addField records a key/value pair as a field. When parent >= 0 it is linked
// to its enclosing table and qualified as `table.key`.
func (w *tomlWalk) addField(pair *tsg.Node, parent int64, parentName string) {
	key := w.firstKey(pair)
	if key == "" {
		return
	}
	qualified := key
	if parentName != "" {
		qualified = parentName + "." + key
	}
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      topology.KindField,
		Name:      key,
		Qualified: qualified,
		StartLine: line(pair.StartPoint()),
		EndLine:   line(pair.EndPoint()),
		Language:  "toml",
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
}

// firstKey returns the leading key of a table header or a pair: a bare key, a
// quoted key (quotes stripped), or a dotted key joined with ".".
func (w *tomlWalk) firstKey(n *tsg.Node) string {
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "bare_key":
			return c.Text(w.src)
		case "quoted_key":
			return strings.Trim(c.Text(w.src), `"'`)
		case "dotted_key":
			return w.dottedKey(c)
		}
	}
	return ""
}

func (w *tomlWalk) dottedKey(n *tsg.Node) string {
	var parts []string
	for _, c := range n.Children() {
		switch c.Type(w.lang) {
		case "bare_key":
			parts = append(parts, c.Text(w.src))
		case "quoted_key":
			parts = append(parts, strings.Trim(c.Text(w.src), `"'`))
		}
	}
	return strings.Join(parts, ".")
}
