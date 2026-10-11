package tools

import (
	"go/ast"
	"go/parser"
	"go/token"
)

// context_samepkg_ast.go — what one Go file says about the call sites in it.
//
// The index records a call as text: the callee, the qualifier to its left and the
// byte it starts at. That cannot tell a bare call of a package-level function from a
// call of a local variable that shares its name, nor say what type a receiver is.
// The file itself can, so the same-package search parses the sibling file the index
// vouches for (its content hash is the one the index parsed) and asks it. It
// resolves nothing across files: a name no declaration of the caller's own function
// accounts for stays unknown, and unknown is never promoted to evidence.

// goFile is one parsed source file and the calls in it by position.
type goFile struct {
	fset    *token.FileSet
	file    *ast.File
	calls   map[int][]*ast.CallExpr // byte offset of a call's first byte -> the calls that start there
	imports map[string]bool         // names the file's imports bind
}

// parseGoFile parses src as a Go file. ok is false when it does not parse: a file the
// compiler would refuse says nothing reliable about its calls.
func parseGoFile(name string, src []byte) (*goFile, bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, false
	}
	g := &goFile{fset: fset, file: f, calls: map[int][]*ast.CallExpr{}, imports: map[string]bool{}}
	for _, imp := range f.Imports {
		if bound := importName(imp); bound != "" {
			g.imports[bound] = true
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			at := g.offset(c.Pos())
			g.calls[at] = append(g.calls[at], c)
		}
		return true
	})
	return g, true
}

func (g *goFile) offset(p token.Pos) int { return g.fset.Position(p).Offset }

// importName is the name an import binds: its alias, else the last element of its
// path. A package whose name differs from that element is not recognised here, which
// only means a call on it reads as a receiver of unknown type.
func importName(imp *ast.ImportSpec) string {
	if imp.Name != nil {
		return imp.Name.Name
	}
	p := imp.Path.Value
	if len(p) < 2 {
		return ""
	}
	p = p[1 : len(p)-1]
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

// callAt is the call that starts at offset and names callee. Several calls can start
// at one byte (`a.b().c()`), which is why the callee, as the index recorded it, picks.
func (g *goFile) callAt(offset int, callee string) *ast.CallExpr {
	for _, c := range g.calls[offset] {
		if calleeName(c.Fun) == callee {
			return c
		}
	}
	return nil
}

// declAt is the top-level declaration holding the byte at offset.
func (g *goFile) declAt(offset int) ast.Decl {
	for _, d := range g.file.Decls {
		if g.offset(d.Pos()) <= offset && offset < g.offset(d.End()) {
			return d
		}
	}
	return nil
}

// calleeName is the name a call expression calls, as the index's Go extractor reads
// it: the identifier, or a selector's right half, through parentheses and a generic
// instantiation.
func calleeName(fun ast.Expr) string {
	switch e := unwrapCallee(fun).(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	}
	return ""
}

// unwrapCallee strips the parentheses and instantiation brackets around a callee.
func unwrapCallee(fun ast.Expr) ast.Expr {
	for {
		switch e := fun.(type) {
		case *ast.ParenExpr:
			fun = e.X
		case *ast.IndexExpr:
			fun = e.X
		case *ast.IndexListExpr:
			fun = e.X
		default:
			return fun
		}
	}
}

// declared is one local declaration of a name inside a top-level declaration: the
// base name of the type the declaration shows, "" when it shows none.
type declared struct{ typ string }

// localDecls lists every place root declares name below its own name: a receiver, a
// parameter or named result (of the declaration or of a function literal in it), a
// `:=`, a var or const, a range variable, a local type. Scope is not tracked, so a
// name declared anywhere in the declaration counts everywhere in it, which can only
// withhold a caller, never invent one.
func localDecls(root ast.Node, name string) []declared {
	var out []declared
	fields := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, f := range fl.List {
			for _, id := range f.Names {
				if id.Name == name {
					out = append(out, declared{typeName(f.Type)})
				}
			}
		}
	}
	ast.Inspect(root, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			fields(x.Recv)
			fields(x.Type.Params)
			fields(x.Type.Results)
		case *ast.FuncLit:
			fields(x.Type.Params)
			fields(x.Type.Results)
		case *ast.AssignStmt:
			if x.Tok == token.DEFINE {
				out = append(out, definedBy(x, name)...)
			}
		case *ast.ValueSpec:
			out = append(out, valueSpecDecls(x, name)...)
		case *ast.RangeStmt:
			out = append(out, rangeDecls(x, name)...)
		case *ast.TypeSpec:
			if x.Name.Name == name {
				out = append(out, declared{})
			}
		}
		return true
	})
	return out
}

// definedBy is the declarations of name a `:=` makes. Its type is known only when
// the right side is a composite literal, its address or new(T) and the sides pair up.
func definedBy(a *ast.AssignStmt, name string) []declared {
	var out []declared
	for i, lhs := range a.Lhs {
		if id, ok := lhs.(*ast.Ident); ok && id.Name == name {
			typ := ""
			if len(a.Rhs) == len(a.Lhs) {
				typ = exprTypeName(a.Rhs[i])
			}
			out = append(out, declared{typ})
		}
	}
	return out
}

func valueSpecDecls(v *ast.ValueSpec, name string) []declared {
	for i, id := range v.Names {
		if id.Name != name {
			continue
		}
		typ := typeName(v.Type)
		if typ == "" && len(v.Values) == len(v.Names) {
			typ = exprTypeName(v.Values[i])
		}
		return []declared{{typ}}
	}
	return nil
}

func rangeDecls(r *ast.RangeStmt, name string) []declared {
	if r.Tok != token.DEFINE {
		return nil
	}
	var out []declared
	for _, e := range []ast.Expr{r.Key, r.Value} {
		if id, ok := e.(*ast.Ident); ok && id.Name == name {
			out = append(out, declared{})
		}
	}
	return out
}

// exprTypeName is the type a value expression visibly has: T{...}, &T{...}, new(T).
func exprTypeName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.ParenExpr:
		return exprTypeName(x.X)
	case *ast.CompositeLit:
		return typeName(x.Type)
	case *ast.UnaryExpr:
		if x.Op == token.AND {
			return exprTypeName(x.X)
		}
	case *ast.CallExpr:
		if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "new" && len(x.Args) == 1 {
			return typeName(x.Args[0])
		}
	}
	return ""
}

// typeName is the base name of a type expression: T for T, *T and T[A]; pkg.T for a
// type of another package. A slice, map, function or interface literal has none.
func typeName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.StarExpr:
		return typeName(x.X)
	case *ast.ParenExpr:
		return typeName(x.X)
	case *ast.IndexExpr:
		return typeName(x.X)
	case *ast.IndexListExpr:
		return typeName(x.X)
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			return id.Name + "." + x.Sel.Name
		}
	}
	return ""
}

// recvEvidence is what the caller's own declarations say about a call's receiver.
type recvEvidence int

const (
	recvUnknown recvEvidence = iota // nothing shows what the receiver is
	recvTyped                       // a declaration in the caller gives it a type
	recvPackage                     // it is an imported package, so this is no method call
)

// receiverOf classifies x, the left side of the selector call x.M(), against the
// declaration holding the call. It returns the receiver's type name for recvTyped.
func (g *goFile) receiverOf(x ast.Expr, decl ast.Decl) (string, recvEvidence) {
	for {
		p, ok := x.(*ast.ParenExpr)
		if !ok {
			break
		}
		x = p.X
	}
	id, ok := x.(*ast.Ident)
	if !ok {
		return "", recvUnknown
	}
	ds := localDecls(decl, id.Name)
	if len(ds) == 0 {
		if g.imports[id.Name] {
			return "", recvPackage
		}
		return "", recvUnknown
	}
	typ := ds[0].typ
	for _, d := range ds {
		if d.typ != typ {
			return "", recvUnknown
		}
	}
	if typ == "" {
		return "", recvUnknown
	}
	return typ, recvTyped
}
