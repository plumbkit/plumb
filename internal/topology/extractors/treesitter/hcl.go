package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// HCLExtractor extracts HashiCorp Configuration Language (Terraform, Packer,
// Nomad, …) symbols using the gotreesitter HCL grammar.
//
// Concurrency: stateless after construction and safe for concurrent use; a
// fresh parser is created per Extract call because gotreesitter parsers are not
// safe for concurrent reuse.
type HCLExtractor struct {
	lang lazyGrammar
}

// NewHCL returns a tree-sitter-backed HCL extractor.
func NewHCL() *HCLExtractor {
	return &HCLExtractor{lang: lazyGrammar{load: grammars.HclLanguage}}
}

func (e *HCLExtractor) Language() string     { return "hcl" }
func (e *HCLExtractor) Extensions() []string { return []string{".tf", ".tfvars", ".hcl"} }

// Extract parses src and returns the top-level HCL blocks as searchable nodes:
// `variable` → variable, `output` and `locals` entries → constant, `resource`/
// `data`/`provider` (and any other labelled block) → type, `module` → import.
// HCL declarations are flat, so no containment edges are emitted. Returns
// (nil, nil, nil) when src cannot be parsed.
func (e *HCLExtractor) Extract(_ context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	tree, err := tsg.NewParser(e.lang.get()).Parse(src)
	if err != nil || tree == nil {
		return nil, nil, nil
	}
	defer tree.Release()
	w := &hclWalk{lang: e.lang.get(), src: src, path: relPath}
	w.walk(tree.RootNode())
	return w.nodes, nil, nil
}

type hclWalk struct {
	lang  *tsg.Language
	src   []byte
	path  string
	nodes []topology.Node
}

func (w *hclWalk) walk(root *tsg.Node) {
	body := childByType(root, "body", w.lang)
	if body == nil {
		return
	}
	for _, n := range body.Children() {
		if n.Type(w.lang) == "block" {
			w.handleBlock(n)
		}
	}
}

// handleBlock classifies a top-level block by its leading identifier and emits
// a node named after its quoted labels.
func (w *hclWalk) handleBlock(n *tsg.Node) {
	id := childByType(n, "identifier", w.lang)
	if id == nil {
		return
	}
	labels := w.labels(n)
	first := ""
	if len(labels) > 0 {
		first = labels[0]
	}
	switch id.Text(w.src) {
	case "variable":
		w.addNamed(n, first, topology.KindVariable)
	case "output":
		w.addNamed(n, first, topology.KindConstant)
	case "data":
		w.addNamed(n, "data."+strings.Join(labels, "."), topology.KindType)
	case "module":
		w.addNamed(n, first, topology.KindImport)
	case "locals":
		w.addLocals(n)
	case "terraform":
		// settings block — no searchable declaration.
	default:
		// resource, provider, and any other labelled block.
		w.addNamed(n, strings.Join(labels, "."), topology.KindType)
	}
}

// labels returns a block's quoted string labels with the surrounding quotes
// stripped, e.g. resource "aws_instance" "web" → ["aws_instance", "web"].
func (w *hclWalk) labels(block *tsg.Node) []string {
	var out []string
	for _, c := range block.Children() {
		if c.Type(w.lang) == "string_lit" {
			out = append(out, strings.Trim(c.Text(w.src), `"`))
		}
	}
	return out
}

// addLocals emits each `name = value` attribute inside a `locals` block as a
// constant.
func (w *hclWalk) addLocals(block *tsg.Node) {
	body := childByType(block, "body", w.lang)
	if body == nil {
		return
	}
	for _, c := range body.Children() {
		if c.Type(w.lang) != "attribute" {
			continue
		}
		if id := childByType(c, "identifier", w.lang); id != nil {
			w.addNamed(c, id.Text(w.src), topology.KindConstant)
		}
	}
}

func (w *hclWalk) addNamed(n *tsg.Node, name string, kind topology.NodeKind) {
	if name == "" {
		return
	}
	w.nodes = append(w.nodes, topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "hcl",
		Path:      w.path,
	})
}
