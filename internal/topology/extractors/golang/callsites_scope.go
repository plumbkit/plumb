package golang

import (
	"go/ast"
	"go/token"
	"strconv"
	"strings"

	"github.com/plumbkit/plumb/internal/topology"
)

// localScope is what one function declaration says about the names it declares:
// how many times each is declared, and, for a name declared exactly once with an
// evident type, that type. It is function-wide, not block-scoped: the index holds
// no scopes, so a name declared twice (a parameter re-declared by a loop variable,
// two := in sibling blocks) is ambiguous everywhere in the function.
type localScope struct {
	count map[string]int
	hints map[string]string
}

// isIdent reports whether a qualifier is a single identifier: a variable or a
// package name, not a field chain or one of the markers.
func isIdent(q string) bool { return token.IsIdentifier(q) }

// declared reports whether the function declares name at all.
func (s localScope) declared(name string) bool { return s.count[name] > 0 }

// receiverType is the CallSite.ReceiverType for a call qualified by name: the
// ambiguity marker for a name declared more than once, the evident type of one
// declared once, or "" when the name is not the function's or nothing is evident.
func (s localScope) receiverType(name string) string {
	switch n := s.count[name]; {
	case n > 1:
		return topology.ReceiverAmbiguous
	case n == 1:
		return s.hints[name]
	}
	return ""
}

// scanLocals counts the function's declarations (receiver, parameters and
// results, and in its body every := and var name, range variable, closure
// parameter or result and local type) and records the evident type of each
// variable a single declaration gives one. Parameters and the receiver carry no
// hint here: their types are in the stored signature, and the resolver reads
// them there.
func scanLocals(d *ast.FuncDecl) localScope {
	s := localScope{count: map[string]int{}, hints: map[string]string{}}
	s.declareFields(d.Recv)
	s.declareFields(d.Type.TypeParams)
	s.declareFields(d.Type.Params)
	s.declareFields(d.Type.Results)
	ast.Inspect(d.Body, func(n ast.Node) bool {
		s.visit(n)
		return true
	})
	s.dropShadowedHints()
	return s
}

// dropShadowedHints removes every hint whose leading identifier the function
// itself declares: `x := open()` with a local closure open, `s := store.Open()`
// with a parameter named store, `var x T` with a local type or a type parameter
// T. Each of those names something the index would resolve to a package-level
// declaration instead, so the hint would mislead; with it gone, the call keeps the
// tiers it had before (a qualifier that is an import still resolves as one).
func (s localScope) dropShadowedHints() {
	for name, hint := range s.hints {
		head := strings.TrimPrefix(hint, topology.ReceiverFromCall)
		if i := strings.IndexAny(head, "./"); i >= 0 {
			head = head[:i]
		}
		if s.count[head] > 0 {
			delete(s.hints, name)
		}
	}
}

func (s localScope) declare(name, hint string) {
	if name == "" || name == "_" {
		return
	}
	s.count[name]++
	if hint != "" {
		s.hints[name] = hint
	}
}

func (s localScope) declareFields(fl *ast.FieldList) {
	if fl == nil {
		return
	}
	for _, f := range fl.List {
		for _, n := range f.Names {
			s.declare(n.Name, "")
		}
	}
}

// visit records the declarations one body node makes.
func (s localScope) visit(n ast.Node) {
	switch st := n.(type) {
	case *ast.AssignStmt:
		if st.Tok != token.DEFINE {
			return
		}
		for i, lhs := range st.Lhs {
			if id, ok := lhs.(*ast.Ident); ok {
				s.declare(id.Name, assignHint(st, i))
			}
		}
	case *ast.ValueSpec:
		for i, id := range st.Names {
			s.declare(id.Name, valueSpecHint(st, i))
		}
	case *ast.RangeStmt:
		if st.Tok != token.DEFINE {
			return
		}
		for _, e := range []ast.Expr{st.Key, st.Value} {
			if id, ok := e.(*ast.Ident); ok {
				s.declare(id.Name, "")
			}
		}
	case *ast.FuncLit:
		s.declareFields(st.Type.Params)
		s.declareFields(st.Type.Results)
	case *ast.TypeSpec:
		s.declare(st.Name.Name, "")
	}
}

// assignHint is the evident type of the i-th name of a := statement.
func assignHint(st *ast.AssignStmt, i int) string {
	if len(st.Lhs) == len(st.Rhs) {
		return exprTypeHint(st.Rhs[i], 0)
	}
	if len(st.Rhs) == 1 {
		switch r := st.Rhs[0].(type) {
		case *ast.CallExpr:
			return exprTypeHint(r, i) // x, err := NewX()
		case *ast.TypeAssertExpr:
			if i == 0 {
				return exprTypeHint(r, 0) // v, ok := x.(T)
			}
		}
	}
	return ""
}

// valueSpecHint is the evident type of the i-th name of `var a, b T = ...`.
func valueSpecHint(st *ast.ValueSpec, i int) string {
	if st.Type != nil {
		return namedTypeText(st.Type)
	}
	if len(st.Values) == len(st.Names) {
		return exprTypeHint(st.Values[i], 0)
	}
	if len(st.Values) == 1 {
		if call, ok := st.Values[0].(*ast.CallExpr); ok {
			return exprTypeHint(call, i)
		}
	}
	return ""
}

// exprTypeHint is what an initialiser says its value's type is: a named type for
// a composite literal, its address, new(T) or an assertion; for a call, the
// called function and the index of the result taken, for the resolver to finish
// from the function's declared results. A conversion `T(x)` reads as a call of
// T; no function named T exists, so the resolver finds nothing and the site stays
// unresolved, which is the safe direction.
func exprTypeHint(e ast.Expr, result int) string {
	switch x := e.(type) {
	case *ast.ParenExpr:
		return exprTypeHint(x.X, result)
	case *ast.CompositeLit:
		return namedTypeText(x.Type)
	case *ast.UnaryExpr:
		if x.Op == token.AND {
			if lit, ok := x.X.(*ast.CompositeLit); ok {
				return namedTypeText(lit.Type)
			}
		}
	case *ast.TypeAssertExpr:
		return namedTypeText(x.Type)
	case *ast.CallExpr:
		fun := x.Fun
		if ix, ok := fun.(*ast.IndexExpr); ok { // New[T]()
			fun = ix.X
		} else if ixl, ok := fun.(*ast.IndexListExpr); ok {
			fun = ixl.X
		}
		if id, ok := fun.(*ast.Ident); ok && id.Name == "new" && len(x.Args) == 1 {
			return namedTypeText(x.Args[0])
		}
		if name := exprText(fun); name != "" && !builtinFunc[name] {
			return topology.ReceiverFromCall + name + "/" + strconv.Itoa(result)
		}
	}
	return ""
}

// namedTypeText renders a type as "T" or "pkg.T", pointers and type arguments
// dropped, or "" for anything that is not a named type.
func namedTypeText(t ast.Expr) string {
	switch x := t.(type) {
	case *ast.StarExpr:
		return namedTypeText(x.X)
	case *ast.ParenExpr:
		return namedTypeText(x.X)
	case *ast.IndexExpr:
		return namedTypeText(x.X)
	case *ast.IndexListExpr:
		return namedTypeText(x.X)
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		if pkg, ok := x.X.(*ast.Ident); ok {
			return pkg.Name + "." + x.Sel.Name
		}
	}
	return ""
}

// builtinFunc names the predeclared functions whose results are never a named
// type of the workspace.
var builtinFunc = map[string]bool{
	"append": true, "cap": true, "clear": true, "close": true, "complex": true, "copy": true,
	"delete": true, "imag": true, "len": true, "make": true, "max": true, "min": true,
	"panic": true, "print": true, "println": true, "real": true, "recover": true,
}
