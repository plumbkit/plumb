package treesitter

import (
	"context"
	"strings"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/golimpio/plumb/internal/topology"
)

// BashExtractor extracts shell symbols using the gotreesitter Bash grammar.
//
// Concurrency: stateless after construction and safe for concurrent use; a
// fresh parser is created per Extract call because gotreesitter parsers are not
// safe for concurrent reuse.
type BashExtractor struct {
	lang lazyGrammar
}

// NewBash returns a tree-sitter-backed shell (bash) extractor.
func NewBash() *BashExtractor {
	return &BashExtractor{lang: lazyGrammar{load: grammars.BashLanguage}}
}

func (e *BashExtractor) Language() string     { return "bash" }
func (e *BashExtractor) Extensions() []string { return []string{".sh", ".bash"} }

// Extract parses src and returns shell functions, top-level variables/constants
// (`readonly`/`declare -r` → constant), `source`/`.` imports, plus intra-file
// call edges between functions. Call sites are syntactically certain but the
// callee is resolved by name within the file, so call edges are heuristic
// (0.8). Returns (nil, nil, nil) when src cannot be parsed.
func (e *BashExtractor) Extract(_ context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	tree, err := tsg.NewParser(e.lang.get()).Parse(src)
	if err != nil || tree == nil {
		return nil, nil, nil
	}
	defer tree.Release()
	w := &bashWalk{lang: e.lang.get(), src: src, path: relPath, funcIdx: map[string]int64{}}
	w.walk(tree.RootNode())
	w.callEdges(tree.RootNode())
	return w.nodes, w.edges, nil
}

type bashWalk struct {
	lang    *tsg.Language
	src     []byte
	path    string
	nodes   []topology.Node
	edges   []topology.Edge
	funcIdx map[string]int64 // function name → node index, for call edges
}

// walk scans the program's direct children: shell declarations are top-level by
// convention, so a flat pass avoids treating function-local `local`/`declare`
// bindings as module-level symbols.
func (w *bashWalk) walk(root *tsg.Node) {
	for _, n := range root.Children() {
		switch n.Type(w.lang) {
		case "function_definition":
			w.addFunc(n)
		case "variable_assignment":
			w.addBinding(n, n, topology.KindVariable)
		case "declaration_command":
			w.handleDecl(n)
		case "command":
			w.maybeImport(n)
		}
	}
}

func (w *bashWalk) addFunc(n *tsg.Node) {
	name := w.funcName(n)
	if name == "" {
		return
	}
	idx := int64(len(w.nodes))
	w.nodes = append(w.nodes, topology.Node{
		Kind:      topology.KindFunction,
		Name:      name,
		Qualified: name,
		StartLine: line(n.StartPoint()),
		EndLine:   line(n.EndPoint()),
		Language:  "bash",
		Path:      w.path,
	})
	w.funcIdx[name] = idx
}

func (w *bashWalk) funcName(n *tsg.Node) string {
	if id := n.ChildByFieldName("name", w.lang); id != nil {
		return id.Text(w.src)
	}
	if id := childByType(n, "word", w.lang); id != nil {
		return id.Text(w.src)
	}
	return ""
}

// handleDecl classifies a `readonly`/`declare`/`export`/`local` declaration:
// readonly (or `declare -r`) bindings are constants, the rest are variables.
func (w *bashWalk) handleDecl(n *tsg.Node) {
	va := childByType(n, "variable_assignment", w.lang)
	if va == nil {
		return
	}
	kind := topology.KindVariable
	if w.isReadonly(n) {
		kind = topology.KindConstant
	}
	w.addBinding(n, va, kind)
}

func (w *bashWalk) isReadonly(n *tsg.Node) bool {
	t := strings.TrimSpace(n.Text(w.src))
	return strings.HasPrefix(t, "readonly") || strings.HasPrefix(t, "declare -r")
}

// addBinding records a variable/constant. lineNode supplies the range (the
// declaration keyword for `readonly X=…`); nameNode supplies the assignment.
func (w *bashWalk) addBinding(lineNode, nameNode *tsg.Node, kind topology.NodeKind) {
	vn := childByType(nameNode, "variable_name", w.lang)
	if vn == nil {
		return
	}
	name := vn.Text(w.src)
	w.nodes = append(w.nodes, topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		StartLine: line(lineNode.StartPoint()),
		EndLine:   line(lineNode.EndPoint()),
		Language:  "bash",
		Path:      w.path,
	})
}

func (w *bashWalk) maybeImport(n *tsg.Node) {
	cn := childByType(n, "command_name", w.lang)
	if cn == nil {
		return
	}
	if name := strings.TrimSpace(cn.Text(w.src)); name != "source" && name != "." {
		return
	}
	target := w.importTarget(n)
	if target == "" {
		return
	}
	w.nodes = append(w.nodes, topology.Node{
		Kind:      topology.KindImport,
		Name:      target,
		StartLine: line(n.StartPoint()),
		Language:  "bash",
		Path:      w.path,
	})
}

// importTarget returns the first argument of a `source`/`.` command — the
// sourced path — stripping surrounding quotes.
func (w *bashWalk) importTarget(cmd *tsg.Node) string {
	for _, c := range cmd.Children() {
		switch c.Type(w.lang) {
		case "word":
			return c.Text(w.src)
		case "string", "raw_string", "concatenation":
			return strings.Trim(c.Text(w.src), `"'`)
		}
	}
	return ""
}

// callEdges does a second pass emitting EdgeCalls between functions defined in
// the file. The call site is certain but the callee is resolved by name within
// the file, so confidence is 0.8 (heuristic).
func (w *bashWalk) callEdges(root *tsg.Node) {
	seen := map[[2]int64]bool{}
	var rec func(n *tsg.Node, curFunc int64)
	rec = func(n *tsg.Node, curFunc int64) {
		switch n.Type(w.lang) {
		case "function_definition":
			if idx, ok := w.funcIdx[w.funcName(n)]; ok {
				curFunc = idx
			}
		case "command":
			w.maybeCallEdge(n, curFunc, seen)
		}
		for _, c := range n.Children() {
			rec(c, curFunc)
		}
	}
	rec(root, -1)
}

func (w *bashWalk) maybeCallEdge(cmd *tsg.Node, curFunc int64, seen map[[2]int64]bool) {
	if curFunc < 0 {
		return
	}
	cn := childByType(cmd, "command_name", w.lang)
	if cn == nil {
		return
	}
	to, ok := w.funcIdx[strings.TrimSpace(cn.Text(w.src))]
	if !ok || to == curFunc {
		return
	}
	key := [2]int64{curFunc, to}
	if seen[key] {
		return
	}
	seen[key] = true
	w.edges = append(w.edges, topology.Edge{
		FromID:     curFunc,
		ToID:       to,
		Kind:       topology.EdgeCalls,
		Confidence: 0.8,
		Source:     "heuristic",
	})
}
