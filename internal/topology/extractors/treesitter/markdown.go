package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/golimpio/plumb/internal/topology"
)

// MarkdownExtractor extracts document headings using the gotreesitter Markdown
// grammar. The grammar nests a `section` per heading, mirroring the heading
// hierarchy, which becomes the containment tree of the outline.
//
// Concurrency: stateless after construction and safe for concurrent use; a
// fresh parser is created per Extract call because gotreesitter parsers are not
// safe for concurrent reuse.
type MarkdownExtractor struct {
	lang lazyGrammar
}

// NewMarkdown returns a tree-sitter-backed Markdown extractor.
func NewMarkdown() *MarkdownExtractor {
	return &MarkdownExtractor{lang: lazyGrammar{load: grammars.MarkdownLanguage}}
}

func (e *MarkdownExtractor) Language() string     { return "markdown" }
func (e *MarkdownExtractor) Extensions() []string { return []string{".md", ".markdown"} }

// Extract parses src and returns each heading as a section node, with a certain
// (1.0) containment edge from a heading to the sub-headings nested beneath it
// (an `## H2` under an `# H1`). Returns (nil, nil, nil) when src cannot be
// parsed.
func (e *MarkdownExtractor) Extract(_ context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	tree, err := tsg.NewParser(e.lang.get()).Parse(src)
	if err != nil || tree == nil {
		return nil, nil, nil
	}
	defer tree.Release()
	w := &mdWalk{lang: e.lang.get(), src: src, path: relPath}
	w.walk(tree.RootNode(), -1)
	return w.nodes, w.edges, nil
}

type mdWalk struct {
	lang  *tsg.Language
	src   []byte
	path  string
	nodes []topology.Node
	edges []topology.Edge
}

// walk emits a node for each child section's heading and recurses into nested
// sections with that heading as their containment parent.
func (w *mdWalk) walk(n *tsg.Node, parent int64) {
	for _, c := range n.Children() {
		if c.Type(w.lang) != "section" {
			continue
		}
		next := parent
		if idx := w.addHeading(c, parent); idx >= 0 {
			next = idx
		}
		w.walk(c, next)
	}
}

func (w *mdWalk) addHeading(section *tsg.Node, parent int64) int64 {
	text := w.headingText(section)
	if text == "" {
		return -1
	}
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      topology.KindSection,
		Name:      text,
		Qualified: text,
		StartLine: line(section.StartPoint()),
		EndLine:   line(section.EndPoint()),
		Language:  "markdown",
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

// headingText returns the trimmed text of a section's leading ATX or setext
// heading, or "" when the section has no heading.
func (w *mdWalk) headingText(section *tsg.Node) string {
	for _, c := range section.Children() {
		if t := c.Type(w.lang); t != "atx_heading" && t != "setext_heading" {
			continue
		}
		if inline := childByType(c, "inline", w.lang); inline != nil {
			return strings.TrimSpace(inline.Text(w.src))
		}
		if para := childByType(c, "paragraph", w.lang); para != nil {
			return strings.TrimSpace(para.Text(w.src))
		}
	}
	return ""
}
