// Package golang provides a Go AST-based topology extractor.
// It uses go/parser and go/ast from the standard library — no CGo required.
// Extraction is syntactic only: no type resolution or import tracing.
//
// Validation status: unit-tested with fixture files.
package golang

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"

	"github.com/plumbkit/plumb/internal/topology"
)

// Extractor extracts Go symbols using the standard go/parser package.
type Extractor struct{}

// New returns a new Go Extractor.
func New() *Extractor { return &Extractor{} }

func (e *Extractor) Language() string     { return "go" }
func (e *Extractor) Extensions() []string { return []string{".go"} }

// Extract parses src as a Go source file and returns nodes and edges.
func (e *Extractor) Extract(_ context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, relPath, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil && f == nil {
		return nil, nil, nil // unrecoverable parse failure — skip file
	}
	return extractFile(fset, f, relPath)
}

// funcEntry pairs a parsed FuncDecl with the index of its Node in the nodes slice.
type funcEntry struct {
	decl *ast.FuncDecl
	idx  int
}

func extractFile(fset *token.FileSet, f *ast.File, relPath string) ([]topology.Node, []topology.Edge, error) {
	var nodes []topology.Node
	var edges []topology.Edge
	var funcEntries []funcEntry

	pkgName := f.Name.Name
	pkgNode := topology.Node{
		Kind:     topology.KindPackage,
		Name:     pkgName,
		Language: "go",
		Path:     relPath,
	}
	nodes = append(nodes, pkgNode)
	pkgIdx := 0

	for _, imp := range f.Imports {
		n := importNode(imp, relPath)
		impIdx := len(nodes)
		nodes = append(nodes, n)
		edges = append(edges, topology.Edge{
			FromID:     int64(pkgIdx),
			ToID:       int64(impIdx),
			Kind:       topology.EdgeImports,
			Confidence: 1.0,
			Source:     "extractor",
		})
	}

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			nodeIdx := len(nodes)
			ns, es := extractFunc(fset, d, relPath, nodeIdx, pkgIdx)
			funcEntries = append(funcEntries, funcEntry{decl: d, idx: nodeIdx})
			nodes = append(nodes, ns...)
			edges = append(edges, es...)
		case *ast.GenDecl:
			ns, es := extractGenDecl(fset, d, relPath, len(nodes), pkgIdx)
			nodes = append(nodes, ns...)
			edges = append(edges, es...)
		}
	}
	edges = append(edges, fileCallEdges(funcEntries, nodes)...)
	return nodes, edges, nil
}

// fileCallEdges emits EdgeCalls for intra-file calls only (confidence 1.0).
// Cross-file calls are not emitted because single-file extraction has no
// information about symbols defined in other files.
func fileCallEdges(funcEntries []funcEntry, nodes []topology.Node) []topology.Edge {
	if len(funcEntries) == 0 {
		return nil
	}
	nameToIdx := buildNameIndex(nodes)
	var edges []topology.Edge
	for _, fe := range funcEntries {
		edges = append(edges, callEdgesFor(fe.decl, int64(fe.idx), nameToIdx)...)
	}
	return edges
}

func buildNameIndex(nodes []topology.Node) map[string]int64 {
	m := make(map[string]int64, len(nodes))
	for i, n := range nodes {
		switch n.Kind {
		case topology.KindFunction, topology.KindMethod, topology.KindTest:
			m[n.Name] = int64(i)
		}
	}
	return m
}

func callEdgesFor(fn *ast.FuncDecl, fromIdx int64, nameToIdx map[string]int64) []topology.Edge {
	if fn.Body == nil {
		return nil
	}
	var edges []topology.Edge
	seen := map[int64]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		callee := calleeIdent(call.Fun)
		toIdx, found := nameToIdx[callee]
		if !found || seen[toIdx] || toIdx == fromIdx {
			return true
		}
		seen[toIdx] = true
		edges = append(edges, topology.Edge{
			FromID:     fromIdx,
			ToID:       toIdx,
			Kind:       topology.EdgeCalls,
			Confidence: 1.0,
			Source:     "extractor",
		})
		return true
	})
	return edges
}

func calleeIdent(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	default:
		return ""
	}
}

func importNode(imp *ast.ImportSpec, relPath string) topology.Node {
	path := strings.Trim(imp.Path.Value, `"`)
	name := filepath.Base(path)
	if imp.Name != nil {
		name = imp.Name.Name
	}
	return topology.Node{
		Kind:      topology.KindImport,
		Name:      name,
		Qualified: path,
		Language:  "go",
		Path:      relPath,
	}
}

func extractFunc(fset *token.FileSet, d *ast.FuncDecl, relPath string, nodeCount, pkgIdx int) ([]topology.Node, []topology.Edge) {
	startPos := fset.Position(d.Pos())
	endPos := fset.Position(d.End())
	kind := topology.KindFunction
	if d.Recv != nil && len(d.Recv.List) > 0 {
		kind = topology.KindMethod
	}
	if isTestFunc(d.Name.Name) {
		kind = topology.KindTest
	}
	sig := funcSignature(d)
	qualified := d.Name.Name
	if d.Recv != nil && len(d.Recv.List) > 0 {
		recv := typeStr(d.Recv.List[0].Type)
		qualified = "(" + recv + ")." + d.Name.Name
	}
	n := topology.Node{
		Kind:      kind,
		Name:      d.Name.Name,
		Qualified: qualified,
		Signature: sig,
		StartLine: startPos.Line,
		EndLine:   endPos.Line,
		Docstring: docComment(d.Doc),
		Language:  "go",
		Path:      relPath,
	}
	setSpan(&n, fset, startPos, endPos, d.Doc)
	nodeIdx := nodeCount
	e := topology.Edge{
		FromID:     int64(pkgIdx),
		ToID:       int64(nodeIdx),
		Kind:       topology.EdgeContains,
		Confidence: 1.0,
		Source:     "extractor",
	}
	return []topology.Node{n}, []topology.Edge{e}
}

func extractGenDecl(fset *token.FileSet, d *ast.GenDecl, relPath string, nodeCount, pkgIdx int) ([]topology.Node, []topology.Edge) {
	var nodes []topology.Node
	var edges []topology.Edge
	for _, spec := range d.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			ns, es := extractTypeSpec(fset, s, d, relPath, nodeCount+len(nodes), pkgIdx)
			nodes = append(nodes, ns...)
			edges = append(edges, es...)
		case *ast.ValueSpec:
			ns := extractValueSpec(fset, s, d, relPath)
			nodes = append(nodes, ns...)
		}
	}
	return nodes, edges
}

func extractTypeSpec(fset *token.FileSet, s *ast.TypeSpec, d *ast.GenDecl, relPath string, nodeCount, pkgIdx int) ([]topology.Node, []topology.Edge) {
	startPos := fset.Position(s.Pos())
	endPos := fset.Position(s.End())
	n := topology.Node{
		Kind:      topology.KindType,
		Name:      s.Name.Name,
		Qualified: s.Name.Name,
		StartLine: startPos.Line,
		EndLine:   endPos.Line,
		Docstring: docComment(d.Doc),
		Language:  "go",
		Path:      relPath,
	}
	// The doc comment belongs to the enclosing GenDecl, but for a single-spec
	// declaration (`// Doc\ntype T struct{…}`) it documents this type; attach it
	// only when the GenDecl wraps exactly one spec, so a grouped `type (…)` block
	// does not mis-assign the block comment to its first member.
	var doc *ast.CommentGroup
	if len(d.Specs) == 1 {
		doc = d.Doc
	}
	setSpan(&n, fset, startPos, endPos, doc)
	e := topology.Edge{
		FromID:     int64(pkgIdx),
		ToID:       int64(nodeCount),
		Kind:       topology.EdgeContains,
		Confidence: 1.0,
		Source:     "extractor",
	}
	return []topology.Node{n}, []topology.Edge{e}
}

func extractValueSpec(fset *token.FileSet, s *ast.ValueSpec, d *ast.GenDecl, relPath string) []topology.Node {
	kind := topology.KindConstant
	if d.Tok.String() == "var" {
		kind = topology.KindVariable
	}
	nodes := make([]topology.Node, 0, len(s.Names))
	for _, name := range s.Names {
		pos := fset.Position(name.Pos())
		end := fset.Position(name.End())
		n := topology.Node{
			Kind:      kind,
			Name:      name.Name,
			Qualified: name.Name,
			StartLine: pos.Line,
			EndLine:   pos.Line,
			Language:  "go",
			Path:      relPath,
		}
		setSpan(&n, fset, pos, end, nil)
		nodes = append(nodes, n)
	}
	return nodes
}

// setSpan records the byte-precise declaration span (and optional doc-comment
// span) on n from the already-computed start/end positions. doc may be nil.
// token.Position offsets are 0-based byte offsets and columns are 1-based, so the
// column is converted to the 0-based convention topology.Node documents.
func setSpan(n *topology.Node, fset *token.FileSet, start, end token.Position, doc *ast.CommentGroup) {
	n.HasBytes = true
	n.StartByte = start.Offset
	n.EndByte = end.Offset
	n.StartCol = col0(start.Column)
	n.EndCol = col0(end.Column)
	if doc != nil {
		n.DocStartByte = fset.Position(doc.Pos()).Offset
		n.DocEndByte = fset.Position(doc.End()).Offset
	}
}

// col0 converts a 1-based token.Position column to the 0-based column convention.
func col0(c int) int {
	if c > 0 {
		return c - 1
	}
	return 0
}

func funcSignature(d *ast.FuncDecl) string {
	var sb strings.Builder
	sb.WriteString("func ")
	if d.Recv != nil && len(d.Recv.List) > 0 {
		sb.WriteString("(")
		sb.WriteString(fieldListStr(d.Recv))
		sb.WriteString(") ")
	}
	sb.WriteString(d.Name.Name)
	sb.WriteString("(")
	if d.Type.Params != nil {
		sb.WriteString(fieldListStr(d.Type.Params))
	}
	sb.WriteString(")")
	if d.Type.Results != nil && len(d.Type.Results.List) > 0 {
		sb.WriteString(" (")
		sb.WriteString(fieldListStr(d.Type.Results))
		sb.WriteString(")")
	}
	return sb.String()
}

func fieldListStr(fl *ast.FieldList) string {
	if fl == nil {
		return ""
	}
	var parts []string
	for _, f := range fl.List {
		parts = append(parts, fieldStr(f))
	}
	return strings.Join(parts, ", ")
}

func fieldStr(f *ast.Field) string {
	var sb strings.Builder
	for i, n := range f.Names {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(n.Name)
	}
	if len(f.Names) > 0 && f.Type != nil {
		sb.WriteString(" ")
	}
	if f.Type != nil {
		sb.WriteString(typeStr(f.Type))
	}
	return sb.String()
}

func typeStr(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + typeStr(t.X)
	case *ast.SelectorExpr:
		return typeStr(t.X) + "." + t.Sel.Name
	case *ast.ArrayType:
		return "[]" + typeStr(t.Elt)
	case *ast.MapType:
		return "map[" + typeStr(t.Key) + "]" + typeStr(t.Value)
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.StructType:
		return "struct{}"
	default:
		return "_"
	}
}

func docComment(cg *ast.CommentGroup) string {
	if cg == nil {
		return ""
	}
	var sb strings.Builder
	for _, c := range cg.List {
		text := strings.TrimPrefix(c.Text, "//")
		text = strings.TrimPrefix(text, "/*")
		text = strings.TrimSuffix(text, "*/")
		sb.WriteString(strings.TrimSpace(text))
		sb.WriteString(" ")
	}
	s := strings.TrimSpace(sb.String())
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}

func isTestFunc(name string) bool {
	return strings.HasPrefix(name, "Test") ||
		strings.HasPrefix(name, "Bench") ||
		strings.HasPrefix(name, "Example")
}
