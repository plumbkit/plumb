package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// funcDecl is one top-level function or method declaration found in the tree.
type funcDecl struct {
	pkg  string // package path relative to the module root
	name string
	pos  string // "file:line", for the failure message
}

// allFuncDecls walks internal/ and cmd/ and returns every top-level func
// declaration.
//
// Test files ARE included: a helper retyped in a _test.go file is still a
// second implementation that will drift from the real one, and the fixtures
// that pinned the old copies were exactly where the duplicates hid. Test
// entry points (TestX, BenchmarkX, FuzzX, ExampleX) are skipped — they are
// named after what they exercise, so "TestTruncateBytes" is not a truncator.
func allFuncDecls(t *testing.T, root string) []funcDecl {
	t.Helper()
	var out []funcDecl
	fset := token.NewFileSet()

	for _, top := range []string{"internal", "cmd"} {
		base := filepath.Join(root, top)
		err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			file, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				t.Fatalf("parsing %s: %v", path, perr)
			}
			rel, rerr := filepath.Rel(root, filepath.Dir(path))
			if rerr != nil {
				t.Fatalf("relativising %s: %v", path, rerr)
			}
			pkg := filepath.ToSlash(rel)
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Name == nil {
					continue
				}
				name := fn.Name.Name
				if isTestEntryPoint(name) {
					continue
				}
				out = append(out, funcDecl{
					pkg:  pkg,
					name: name,
					pos:  filepath.Join(pkg, filepath.Base(path)) + ":" + fsetLine(fset, fn.Pos()),
				})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", base, err)
		}
	}
	return out
}

func isTestEntryPoint(name string) bool {
	for _, p := range []string{"Test", "Benchmark", "Fuzz", "Example"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func fsetLine(fset *token.FileSet, pos token.Pos) string {
	return strconv.Itoa(fset.Position(pos).Line)
}

// matches reports whether a function name falls under the rule's shape.
func (r PrimitiveRule) matches(name string) bool {
	lower := strings.ToLower(name)
	for _, p := range r.Prefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	for _, e := range r.Exact {
		if lower == e {
			return true
		}
	}
	return false
}

// TestSharedPrimitives fails when a helper the tree keeps in one place has been
// reimplemented somewhere else without a declared reason.
//
// If this fails on code you just wrote: use the helper in the rule's Home
// package. If your function genuinely differs, add it to that rule's Allowed
// map with a sentence saying how — the entry is the review record.
func TestSharedPrimitives(t *testing.T) {
	root := repoRoot(t)
	decls := allFuncDecls(t, root)

	for _, rule := range PrimitiveRules {
		if _, ok := Layers[rule.Home]; !ok {
			t.Errorf("rule %q names Home %q, which is not a package in Layers",
				rule.Primitive, rule.Home)
		}
		for _, d := range decls {
			if d.pkg == rule.Home || !rule.matches(d.name) {
				continue
			}
			key := d.pkg + "." + d.name
			reason, allowed := rule.Allowed[key]
			switch {
			case !allowed:
				t.Errorf("%s: %s reimplements %s, which belongs in %s.\n"+
					"    Use the %s helper, or add %q to the rule's Allowed map with the reason it differs.",
					d.pos, d.name, rule.Primitive, rule.Home, rule.Home, key)
			case strings.TrimSpace(reason) == "":
				t.Errorf("%s: allowlist entry %q has no reason recorded", d.pos, key)
			}
		}
	}
}

// TestPrimitiveAllowlistsAreLive catches the other drift direction: an
// allowlist entry for a function that no longer exists. A stale exemption
// silently re-opens the hole it was written to document.
func TestPrimitiveAllowlistsAreLive(t *testing.T) {
	root := repoRoot(t)
	decls := allFuncDecls(t, root)

	present := map[string]bool{}
	for _, d := range decls {
		present[d.pkg+"."+d.name] = true
	}

	for _, rule := range PrimitiveRules {
		for key := range rule.Allowed {
			if !present[key] {
				t.Errorf("rule %q allowlists %q, but no such function exists — remove the entry",
					rule.Primitive, key)
			}
		}
	}
}

// callSite is one qualified call found inside a named function.
type callSite struct {
	pkg  string
	fn   string
	call string
	pos  string
}

// allCallSites walks internal/ and cmd/ and records every "pkg.Func(...)"
// selector call, attributed to the function that makes it.
//
// Test files are excluded here, unlike allFuncDecls: a fixture that builds a
// torn file on purpose, or a test that renames something to set up a case, is
// not a durability hazard. The rules protect the shipped write paths.
func allCallSites(t *testing.T, root string) []callSite {
	t.Helper()
	var out []callSite
	fset := token.NewFileSet()

	for _, top := range []string{"internal", "cmd"} {
		base := filepath.Join(root, top)
		err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				t.Fatalf("parsing %s: %v", path, perr)
			}
			rel, _ := filepath.Rel(root, filepath.Dir(path))
			pkg := filepath.ToSlash(rel)

			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Name == nil || fn.Body == nil {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					ident, ok := sel.X.(*ast.Ident)
					if !ok {
						return true
					}
					out = append(out, callSite{
						pkg:  pkg,
						fn:   fn.Name.Name,
						call: ident.Name + "." + sel.Sel.Name,
						pos:  filepath.Join(pkg, filepath.Base(path)) + ":" + fsetLine(fset, call.Pos()),
					})
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", base, err)
		}
	}
	return out
}

// TestPinnedCalls fails when a call that belongs behind a shared helper is
// open-coded somewhere new.
//
// This is the rule that would have caught the atomic write spreading. Nobody
// declared a second AtomicWrite — they wrote another CreateTemp/Write/Rename
// sequence inline, which no name-based check can see.
func TestPinnedCalls(t *testing.T) {
	root := repoRoot(t)
	sites := allCallSites(t, root)

	for _, rule := range CallRules {
		for _, s := range sites {
			if s.call != rule.Call {
				continue
			}
			key := s.pkg + "." + s.fn
			reason, allowed := rule.Allowed[key]
			switch {
			case !allowed:
				t.Errorf("%s: %s calls %s directly.\n    %s\n    If this really is a different case, add %q to the rule's Allowed map with the reason.",
					s.pos, s.fn, rule.Call, rule.Why, key)
			case strings.TrimSpace(reason) == "":
				t.Errorf("%s: allowlist entry %q has no reason recorded", s.pos, key)
			}
		}
	}
}

// TestPinnedCallAllowlistsAreLive is the companion to
// TestPrimitiveAllowlistsAreLive: an exemption for a call site that no longer
// exists silently re-opens the hole it documented.
func TestPinnedCallAllowlistsAreLive(t *testing.T) {
	root := repoRoot(t)
	sites := allCallSites(t, root)

	for _, rule := range CallRules {
		seen := map[string]bool{}
		for _, s := range sites {
			if s.call == rule.Call {
				seen[s.pkg+"."+s.fn] = true
			}
		}
		for key := range rule.Allowed {
			if !seen[key] {
				t.Errorf("rule %q allowlists %q, but it no longer calls %s — remove the entry",
					rule.Call, key, rule.Call)
			}
		}
	}
}
