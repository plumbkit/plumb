package tools

import (
	"go/ast"
	"strings"
	"testing"
)

// context_samepkg_ast_test.go — what a parsed sibling file may say about a call: the
// call it is, what shadows a name, and what type a receiver is shown to have.

func parseSnippet(t *testing.T, src string) *goFile {
	t.Helper()
	g, ok := parseGoFile("x.go", []byte(src))
	if !ok {
		t.Fatalf("the snippet does not parse:\n%s", src)
	}
	return g
}

func declNamed(t *testing.T, g *goFile, name string) ast.Decl {
	t.Helper()
	for _, d := range g.file.Decls {
		if f, ok := d.(*ast.FuncDecl); ok && f.Name.Name == name {
			return f
		}
	}
	t.Fatalf("no function %s in the snippet", name)
	return nil
}

func TestParseGoFile_RefusesWhatDoesNotParse(t *testing.T) {
	if _, ok := parseGoFile("x.go", []byte("package p\n\nfunc broken( {")); ok {
		t.Error("a file that does not parse was taken as evidence")
	}
}

// Several calls can start at one byte, and the index records each of them; the callee
// it recorded is what tells them apart.
func TestGoFile_CallAtPicksTheCallByItsCalleeWhenSeveralStartTogether(t *testing.T) {
	src := "package p\n\nfunc f() { a.b().c(); New[int](1) }\n"
	g := parseSnippet(t, src)
	at := strings.Index(src, "a.b()")
	inner, outer := g.callAt(at, "b"), g.callAt(at, "c")
	if inner == nil || outer == nil || inner == outer {
		t.Fatalf("callAt(b) = %v, callAt(c) = %v; want two different calls", inner, outer)
	}
	if calleeName(inner.Fun) != "b" || calleeName(outer.Fun) != "c" {
		t.Errorf("the calls are %q and %q, want b and c", calleeName(inner.Fun), calleeName(outer.Fun))
	}
	if g.callAt(at, "d") != nil {
		t.Error("a callee nothing at that byte calls matched a call")
	}
	if c := g.callAt(strings.Index(src, "New["), "New"); c == nil || calleeName(c.Fun) != "New" {
		t.Error("a generic instantiation is not read as a call of its function")
	}
}

func TestGoFile_DeclAtIsTheTopLevelDeclarationHoldingTheByte(t *testing.T) {
	src := "package p\n\nvar v = f()\n\nfunc f() int { return g() }\n\nfunc g() int { return 1 }\n"
	g := parseSnippet(t, src)
	if d, ok := g.declAt(strings.Index(src, "g() }")).(*ast.FuncDecl); !ok || d.Name.Name != "f" {
		t.Errorf("the call of g in f is held by %v", g.declAt(strings.Index(src, "g() }")))
	}
	if _, ok := g.declAt(strings.Index(src, "f()\n")).(*ast.GenDecl); !ok {
		t.Error("a package-level initialiser is not held by its var declaration")
	}
	if g.declAt(0) != nil {
		t.Error("the package clause was given a declaration")
	}
}

func TestLocalDecls(t *testing.T) {
	src := `package p

func recv(x *T) {}
func param(a int, x T) {}
func result() (x T) { return }
func short() { x := &T{}; _ = x }
func varDecl() { var x pkg.T; _ = x }
func varInit() { var x = new(T); _ = x }
func ranged(s []T) { for _, x := range s { _ = x } }
func closure() { func(x U) {}(nil) }
func localType() { type x struct{}; _ = x }
func none(a int) { _ = a }
func two() { x := T{}; { x := U{}; _ = x }; _ = x }
func (x V) method() {}
func unknown() { x := f(); _ = x }
func assignOnly() { var x T; x = U{}; _ = x }
`
	g := parseSnippet(t, src)
	for _, tc := range []struct {
		fn   string
		want []string // the type each declaration shows, "" when it shows none
	}{
		{"recv", []string{"T"}},
		{"param", []string{"T"}},
		{"result", []string{"T"}},
		{"short", []string{"T"}},
		{"varDecl", []string{"pkg.T"}},
		{"varInit", []string{"T"}},
		{"ranged", []string{""}},
		{"closure", []string{"U"}},
		{"localType", []string{""}},
		{"none", nil},
		{"two", []string{"T", "U"}},
		{"method", []string{"V"}},
		{"unknown", []string{""}},
		{"assignOnly", []string{"T"}},
	} {
		var got []string
		for _, d := range localDecls(declNamed(t, g, tc.fn), "x") {
			got = append(got, d.typ)
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: declarations of x show types %q, want %q", tc.fn, got, tc.want)
		}
	}
}

func TestReceiverOf(t *testing.T) {
	src := `package p

import (
	"strings"
	str "bytes"
)

func typed(c *Cart) { c.Total() }
func conflict() { c := Cart{}; { c := Other{}; _ = c }; c.Total() }
func untyped() { c := build(); c.Total() }
func pkg() { strings.Total() }
func aliased() { str.Total() }
func unseen() { global.Total() }
func chain() { build().Total() }
func shadowedPkg(strings Cart) { strings.Total() }
func paren() { c := &Cart{}; (c).Total() }
`
	g := parseSnippet(t, src)
	for _, tc := range []struct {
		fn   string
		typ  string
		want recvEvidence
	}{
		{"typed", "Cart", recvTyped},
		{"conflict", "", recvUnknown}, // two declarations disagree: nothing is shown
		{"untyped", "", recvUnknown},
		{"pkg", "", recvPackage},
		{"aliased", "", recvPackage},
		{"unseen", "", recvUnknown},
		{"chain", "", recvUnknown},
		{"shadowedPkg", "Cart", recvTyped}, // a local declaration beats the import
		{"paren", "Cart", recvTyped},
	} {
		decl := declNamed(t, g, tc.fn).(*ast.FuncDecl)
		var sel *ast.SelectorExpr
		ast.Inspect(decl.Body, func(n ast.Node) bool {
			if s, ok := n.(*ast.SelectorExpr); ok && s.Sel.Name == "Total" {
				sel = s
			}
			return true
		})
		if sel == nil {
			t.Fatalf("%s: no x.Total() in the snippet", tc.fn)
		}
		if typ, ev := g.receiverOf(sel.X, decl); typ != tc.typ || ev != tc.want {
			t.Errorf("%s: receiver = (%q, %d), want (%q, %d)", tc.fn, typ, ev, tc.typ, tc.want)
		}
	}
}
