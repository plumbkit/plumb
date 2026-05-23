package langsupport

import "testing"

func TestByPath(t *testing.T) {
	cases := map[string]string{
		"main.go":       "go",
		"pkg/app.py":    "python",
		"src/index.ts":  "typescript",
		"Component.tsx": "typescript",
		"server.mjs":    "typescript",
		"Main.java":     "java",
		"README.md":     "",
		"Makefile":      "",
	}
	for path, want := range cases {
		l, ok := ByPath(path)
		if want == "" {
			if ok {
				t.Errorf("ByPath(%q) = %q, want no match", path, l.Name)
			}
			continue
		}
		if !ok || l.Name != want {
			t.Errorf("ByPath(%q) = (%q, %v), want %q", path, l.Name, ok, want)
		}
	}
}

func TestByName(t *testing.T) {
	if l, ok := ByName("go"); !ok || l.Structural != EngineNativeAST {
		t.Errorf("ByName(go) = (%+v, %v), want EngineNativeAST", l, ok)
	}
	if l, ok := ByName("python"); !ok || l.LSPAdapter != "pyright-langserver" {
		t.Errorf("ByName(python) = (%+v, %v), want pyright-langserver", l, ok)
	}
	if _, ok := ByName("cobol"); ok {
		t.Error("ByName(cobol) should not be found")
	}
}

// TestRegistryConsistency guards the registry invariants: non-empty names and
// dot-prefixed, unambiguously-owned extensions.
func TestRegistryConsistency(t *testing.T) {
	owner := map[string]string{}
	for _, l := range All() {
		if l.Name == "" {
			t.Error("registry entry with empty name")
		}
		for _, e := range l.Extensions {
			if e == "" || e[0] != '.' {
				t.Errorf("%s: extension %q must be dot-prefixed", l.Name, e)
			}
			if prev, dup := owner[e]; dup {
				t.Errorf("extension %q owned by both %s and %s", e, prev, l.Name)
			}
			owner[e] = l.Name
		}
	}
}
