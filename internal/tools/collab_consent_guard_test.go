package tools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestConsentGuard_NoReaderGatesOnCrossProject keeps cross_project consent out
// of the readers' hands (#635). The daemon-level store holds this repository
// family's own mail as well as other projects', and only its read scope
// (collab/scope.go, via Claimant.CrossProject) can tell them apart. A reader
// that goes back to `if policy.CrossProject { read the global store }` silently
// drops same-project mail for every project that never opted in — the wake hook
// and the preview stop seeing it while check_messages still delivers it, or the
// other way round. So no `if` in this package or internal/cli may branch on the
// consent flag; the flag travels into a Claimant and the store decides.
//
// allowed lists the conditions that are not reads: wording that depends on
// the setting.
func TestConsentGuard_NoReaderGatesOnCrossProject(t *testing.T) {
	allowed := map[string]bool{
		"check_messages.go:empty": true, // the "cross_project is off" sentence
	}
	found := map[string]bool{}
	scanned := 0
	for _, dir := range []string{".", filepath.Join("..", "cli")} {
		fset := token.NewFileSet()
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			scanned++
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				site := filepath.Base(path) + ":" + fn.Name.Name
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					stmt, ok := n.(*ast.IfStmt)
					if !ok || !mentionsConsentFlag(stmt.Cond) {
						return true
					}
					found[site] = true
					if !allowed[site] {
						t.Errorf("%s branches on CrossProject at %s — pass the flag in a collab.Claimant and let the store's read scope decide",
							site, fset.Position(stmt.Pos()))
					}
					return true
				})
			}
		}
	}
	if scanned < 50 {
		t.Fatalf("only %d files scanned; the guard is not looking at the packages", scanned)
	}
	// Positive control: the allowed site exists and is detected, so the walk is
	// genuinely finding consent conditions rather than passing vacuously.
	for site := range allowed {
		if !found[site] {
			t.Errorf("allowed site %s no longer branches on CrossProject — drop it from the list", site)
		}
	}
}

// mentionsConsentFlag reports whether expr reads a field named CrossProject.
// TargetAllowsCrossProject — the SEND side's question about the recipient — is a
// different identifier and is not matched.
func mentionsConsentFlag(expr ast.Expr) bool {
	hit := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "CrossProject" {
			hit = true
		}
		return !hit
	})
	return hit
}
