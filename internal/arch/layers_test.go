package arch

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const modulePath = "github.com/plumbkit/plumb"

// repoRoot resolves the module root from this test's package directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected go.mod at %s: %v", root, err)
	}
	return root
}

// firstPartyImports walks internal/ and cmd/ and returns, per package (a path
// relative to the module root), the set of first-party packages it imports.
//
// Test files are deliberately excluded. A test may legitimately reach upward —
// an integration harness in a low layer wiring a real CLI, say — and the
// architectural constraint that matters is the one on the shipped binary's
// dependency graph. Build-tagged files ARE included: go/parser ignores build
// constraints, which is what we want, since an integration-only import inverts
// the layering just as thoroughly as a normal one.
func firstPartyImports(t *testing.T, root string) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	fset := token.NewFileSet()

	for _, top := range []string{"internal", "cmd"} {
		base := filepath.Join(root, top)
		err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// testdata holds fixture sources that are not part of the build.
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}

			f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if perr != nil {
				return perr
			}

			rel, rerr := filepath.Rel(root, filepath.Dir(path))
			if rerr != nil {
				return rerr
			}
			pkg := filepath.ToSlash(rel)
			if out[pkg] == nil {
				out[pkg] = map[string]bool{}
			}
			for _, spec := range f.Imports {
				imp := strings.Trim(spec.Path.Value, `"`)
				if !strings.HasPrefix(imp, modulePath+"/") {
					continue
				}
				out[pkg][strings.TrimPrefix(imp, modulePath+"/")] = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", base, err)
		}
	}
	return out
}

// TestLayering is the enforcement AGENTS.md's architecture section never had: no
// package may import a package in a higher layer.
func TestLayering(t *testing.T) {
	root := repoRoot(t)
	imports := firstPartyImports(t, root)

	for pkg, deps := range imports {
		from, ok := Layers[pkg]
		if !ok {
			continue // reported by TestEveryPackageHasALayer; don't double-fail
		}
		for dep := range deps {
			to, ok := Layers[dep]
			if !ok {
				continue
			}
			if to > from {
				t.Errorf("layering violation: %s (%s) imports %s (%s)\n"+
					"    A %s package must not depend on a %s one. Either move the shared\n"+
					"    type down into a lower layer, or invert the dependency with an\n"+
					"    interface declared in %s.",
					pkg, from, dep, to, from, to, pkg)
			}
		}
	}
}

// TestEveryPackageHasALayer fails when a package exists on disk but is absent
// from Layers. This is the load-bearing half of the pair: without it a new
// package silently escapes the layering rule entirely, and TestLayering would
// keep passing while the architecture quietly stopped being true.
func TestEveryPackageHasALayer(t *testing.T) {
	root := repoRoot(t)
	imports := firstPartyImports(t, root)

	for pkg := range imports {
		if _, ok := Layers[pkg]; !ok {
			t.Errorf("package %s has no entry in arch.Layers\n"+
				"    Add it to internal/arch/layers.go, choosing the layer deliberately:\n"+
				"    foundation < transport < domain < intelligence < application < presentation.",
				pkg)
		}
	}
}

// TestNoStaleLayerEntries is the reverse guard — a Layers entry for a package
// that no longer exists is misleading documentation, and would let a future
// reader believe a deleted package's placement was still meaningful.
func TestNoStaleLayerEntries(t *testing.T) {
	root := repoRoot(t)
	for pkg := range Layers {
		if _, err := os.Stat(filepath.Join(root, pkg)); err != nil {
			t.Errorf("arch.Layers lists %s, which does not exist on disk — remove the entry", pkg)
		}
	}
}

// TestFoundationIsSelfContained pins the property that makes the bottom layer
// useful: foundation packages may only import each other. If a foundation
// package reaches into transport or domain, it is not foundation any more and
// every layer above inherits the extra coupling.
func TestFoundationIsSelfContained(t *testing.T) {
	root := repoRoot(t)
	imports := firstPartyImports(t, root)

	for pkg, deps := range imports {
		if Layers[pkg] != LayerFoundation {
			continue
		}
		for dep := range deps {
			layer, ok := Layers[dep]
			if !ok {
				continue
			}
			if layer != LayerFoundation {
				t.Errorf("foundation package %s imports %s (%s) — foundation must be self-contained",
					pkg, dep, layer)
			}
		}
	}
}
