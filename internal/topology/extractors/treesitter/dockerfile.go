package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/golimpio/plumb/internal/topology"
)

// DockerfileExtractor extracts Dockerfile symbols using the gotreesitter
// Dockerfile grammar. Its Extensions are bare filename stems (not dot-prefixed)
// so it matches `Dockerfile`, `Dockerfile.prod`, `prod.dockerfile` and
// `Containerfile` — Dockerfiles usually have no file extension.
//
// Concurrency: stateless after construction and safe for concurrent use; a
// fresh parser is created per Extract call because gotreesitter parsers are not
// safe for concurrent reuse.
type DockerfileExtractor struct {
	lang lazyGrammar
}

// NewDockerfile returns a tree-sitter-backed Dockerfile extractor.
func NewDockerfile() *DockerfileExtractor {
	return &DockerfileExtractor{lang: lazyGrammar{load: grammars.DockerfileLanguage}}
}

func (e *DockerfileExtractor) Language() string     { return "dockerfile" }
func (e *DockerfileExtractor) Extensions() []string { return []string{"dockerfile", "containerfile"} }

// Extract parses src and returns each build stage (`FROM …`, named after its
// `AS` alias or base image) as a type, and the `ENV`/`ARG` declarations that
// follow it as variables contained in that stage (containment is lexical and
// certain, 1.0). Returns (nil, nil, nil) when src cannot be parsed.
func (e *DockerfileExtractor) Extract(_ context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	tree, err := tsg.NewParser(e.lang.get()).Parse(src)
	if err != nil || tree == nil {
		return nil, nil, nil
	}
	defer tree.Release()
	w := &dockerWalk{lang: e.lang.get(), src: src, path: relPath}
	w.walk(tree.RootNode())
	return w.nodes, w.edges, nil
}

type dockerWalk struct {
	lang  *tsg.Language
	src   []byte
	path  string
	nodes []topology.Node
	edges []topology.Edge
}

// walk scans top-level instructions, tracking the current build stage so that
// ENV/ARG bindings attach to the FROM that introduced them.
func (w *dockerWalk) walk(root *tsg.Node) {
	curStage := int64(-1)
	for _, n := range root.Children() {
		switch n.Type(w.lang) {
		case "from_instruction":
			curStage = w.addStage(n)
		case "env_instruction":
			w.addEnvPairs(n, curStage)
		case "arg_instruction":
			w.addArg(n, curStage)
		}
	}
}

func (w *dockerWalk) addStage(n *tsg.Node) int64 {
	name := w.stageName(n)
	if name == "" {
		return -1
	}
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      topology.KindType,
		Name:      name,
		Qualified: name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "dockerfile",
		Path:      w.path,
	})
	return idx
}

// stageName is the `AS` alias when present, else the base image name.
func (w *dockerWalk) stageName(n *tsg.Node) string {
	if alias := childByType(n, "image_alias", w.lang); alias != nil {
		return alias.Text(w.src)
	}
	if spec := childByType(n, "image_spec", w.lang); spec != nil {
		if name := childByType(spec, "image_name", w.lang); name != nil {
			return name.Text(w.src)
		}
	}
	return ""
}

func (w *dockerWalk) addEnvPairs(n *tsg.Node, stage int64) {
	for _, c := range n.Children() {
		if c.Type(w.lang) != "env_pair" {
			continue
		}
		if key := firstNamedChild(c); key != nil {
			w.addVar(c, key.Text(w.src), stage)
		}
	}
}

func (w *dockerWalk) addArg(n *tsg.Node, stage int64) {
	if name := firstNamedChild(n); name != nil {
		w.addVar(n, name.Text(w.src), stage)
	}
}

// addVar records an ENV/ARG binding. A `NAME=value` token is split on the first
// `=` so only the name is indexed; the binding is linked to its stage.
func (w *dockerWalk) addVar(n *tsg.Node, raw string, stage int64) {
	name := strings.SplitN(raw, "=", 2)[0]
	if name == "" {
		return
	}
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      topology.KindVariable,
		Name:      name,
		Qualified: name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "dockerfile",
		Path:      w.path,
	})
	if stage >= 0 {
		w.edges = append(w.edges, topology.Edge{
			FromID:     stage,
			ToID:       idx,
			Kind:       topology.EdgeContains,
			Confidence: 1.0,
			Source:     "extractor",
		})
	}
}
