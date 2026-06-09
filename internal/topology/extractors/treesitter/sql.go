package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// SQLExtractor extracts SQL data-definition symbols (tables, views, indexes and
// table columns) using the gotreesitter SQL grammar. Data-manipulation
// statements (INSERT/SELECT/UPDATE/DELETE) are operations, not declarations, so
// they are not indexed.
//
// Concurrency: stateless after construction and safe for concurrent use; a
// fresh parser is created per Extract call because gotreesitter parsers are not
// safe for concurrent reuse.
type SQLExtractor struct {
	lang lazyGrammar
}

// NewSQL returns a tree-sitter-backed SQL extractor.
func NewSQL() *SQLExtractor {
	return &SQLExtractor{lang: lazyGrammar{load: grammars.SqlLanguage}}
}

func (e *SQLExtractor) Language() string     { return "sql" }
func (e *SQLExtractor) Extensions() []string { return []string{".sql"} }

// Extract parses src and returns CREATE TABLE/VIEW/INDEX (and other CREATE)
// statements as types, with each table's columns as fields linked by a certain
// (1.0) containment edge. Returns (nil, nil, nil) when src cannot be parsed.
func (e *SQLExtractor) Extract(_ context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	tree, err := tsg.NewParser(e.lang.get()).Parse(src)
	if err != nil || tree == nil {
		return nil, nil, nil
	}
	defer tree.Release()
	w := &sqlWalk{lang: e.lang.get(), src: src, path: relPath}
	w.walk(tree.RootNode())
	return w.nodes, w.edges, nil
}

type sqlWalk struct {
	lang  *tsg.Language
	src   []byte
	path  string
	nodes []topology.Node
	edges []topology.Edge
}

func (w *sqlWalk) walk(root *tsg.Node) {
	for _, n := range root.Children() {
		t := n.Type(w.lang)
		switch {
		case t == "create_table_statement":
			w.addTable(n)
		case strings.HasPrefix(t, "create_") && strings.HasSuffix(t, "_statement"):
			// create_view_statement, create_index_statement, create_*… —
			// the first identifier is the created object's name.
			w.addNamed(n, w.firstIdent(n), topology.KindType, "")
		}
	}
}

// addTable records the table as a type and each of its columns as a contained
// field.
func (w *sqlWalk) addTable(n *tsg.Node) {
	name := w.firstIdent(n)
	if name == "" {
		return
	}
	tableIdx := int64(len(w.nodes))
	w.addNamed(n, name, topology.KindType, "")
	params := childByType(n, "table_parameters", w.lang)
	if params == nil {
		return
	}
	for _, c := range params.Children() {
		if c.Type(w.lang) != "table_column" {
			continue
		}
		col := w.firstIdent(c)
		if col == "" {
			continue
		}
		colIdx := int64(len(w.nodes))
		w.addNamed(c, col, topology.KindField, name+"."+col)
		w.edges = append(w.edges, topology.Edge{
			FromID:     tableIdx,
			ToID:       colIdx,
			Kind:       topology.EdgeContains,
			Confidence: 1.0,
			Source:     "extractor",
		})
	}
}

func (w *sqlWalk) firstIdent(n *tsg.Node) string {
	if id := childByType(n, "identifier", w.lang); id != nil {
		return id.Text(w.src)
	}
	return ""
}

// addNamed appends a node; qualified defaults to name when empty.
func (w *sqlWalk) addNamed(n *tsg.Node, name string, kind topology.NodeKind, qualified string) {
	if name == "" {
		return
	}
	if qualified == "" {
		qualified = name
	}
	w.nodes = append(w.nodes, topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: qualified,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "sql",
		Path:      w.path,
	})
}
