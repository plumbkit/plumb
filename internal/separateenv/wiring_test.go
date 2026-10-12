package separateenv

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// TestGuardIsFirstInEachDaemonStartingTestMain pins the wiring the library
// tests cannot: CI and the verify container both count as separate
// environments, so deleting a guard call would leave every run green. Each
// suite that starts plumb daemons must call RequireForTestMain as the first
// statement of its TestMain, before anything is built or created.
func TestGuardIsFirstInEachDaemonStartingTestMain(t *testing.T) {
	for _, file := range []string{
		filepath.Join("..", "..", "cmd", "smoke", "main_test.go"),
		filepath.Join("..", "..", "cmd", "clientsmoke", "harness_test.go"),
	} {
		t.Run(file, func(t *testing.T) {
			f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			var main *ast.FuncDecl
			for _, decl := range f.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "TestMain" {
					main = fn
				}
			}
			if main == nil || main.Body == nil || len(main.Body.List) == 0 {
				t.Fatal("no TestMain with a body")
			}
			stmt, ok := main.Body.List[0].(*ast.ExprStmt)
			if !ok {
				t.Fatal("the first statement of TestMain is not a call")
			}
			call, ok := stmt.X.(*ast.CallExpr)
			if !ok {
				t.Fatal("the first statement of TestMain is not a call")
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			pkg, pkgOK := func() (*ast.Ident, bool) {
				if !ok {
					return nil, false
				}
				id, ok := sel.X.(*ast.Ident)
				return id, ok
			}()
			if !ok || !pkgOK || pkg.Name != "separateenv" || sel.Sel.Name != "RequireForTestMain" {
				t.Fatalf("the first statement of TestMain must be separateenv.RequireForTestMain(...)")
			}
		})
	}
}
