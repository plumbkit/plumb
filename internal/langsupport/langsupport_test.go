package langsupport

import (
	"strings"
	"testing"
)

func TestByPath(t *testing.T) {
	cases := map[string]string{
		"main.go":           "go",
		"pkg/app.py":        "python",
		"src/index.ts":      "typescript",
		"Component.tsx":     "typescript",
		"server.mjs":        "typescript",
		"Main.java":         "java",
		"lib/core.rs":       "rust",
		"src/main.zig":      "zig",
		"app/Main.kt":       "kotlin",
		"build.gradle.kts":  "kotlin",
		"Sources/App.swift": "swift",
		"scripts/run.sh":    "bash",
		"deploy.bash":       "bash",
		"infra/main.tf":     "hcl",
		"vars.tfvars":       "hcl",
		"schema/init.sql":   "sql",
		"Dockerfile":        "dockerfile",
		"Dockerfile.prod":   "dockerfile",
		"app.dockerfile":    "dockerfile",
		"Containerfile":     "dockerfile",
		"Cargo.toml":        "toml",
		"k8s/deploy.yaml":   "yaml",
		"ci.yml":            "yaml",
		"README.md":         "",
		"Makefile":          "",
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
	// adapter "" means "don't check the adapter".
	cases := []struct {
		name    string
		engine  StructuralEngine
		adapter string
	}{
		{"go", EngineNativeAST, "gopls"},
		{"python", EngineTreeSitter, "pyright-langserver"},
		{"rust", EngineTreeSitter, ""},
		{"zig", EngineTreeSitter, ""},
		{"kotlin", EngineTreeSitter, ""},
		{"swift", EngineTreeSitter, ""},
		{"bash", EngineTreeSitter, ""},
		{"hcl", EngineTreeSitter, ""},
		{"sql", EngineTreeSitter, ""},
		{"dockerfile", EngineTreeSitter, ""},
		{"toml", EngineTreeSitter, ""},
		{"yaml", EngineTreeSitter, ""},
		{"java", EngineTreeSitter, "jdtls"}, // tree-sitter Map + jdtls GPS
	}
	for _, c := range cases {
		l, ok := ByName(c.name)
		if !ok || l.Structural != c.engine {
			t.Errorf("ByName(%s) = (%+v, %v), want engine %v", c.name, l, ok, c.engine)
		}
		if c.adapter != "" && l.LSPAdapter != c.adapter {
			t.Errorf("ByName(%s) adapter = %q, want %q", c.name, l.LSPAdapter, c.adapter)
		}
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
			// Patterns are either dot-prefixed extensions (".go") or bare
			// filename stems ("dockerfile"); a bare stem must not contain a dot.
			switch {
			case e == "":
				t.Errorf("%s: empty extension", l.Name)
			case e[0] != '.' && strings.Contains(e, "."):
				t.Errorf("%s: bare filename pattern %q must not contain a dot", l.Name, e)
			}
			if prev, dup := owner[e]; dup {
				t.Errorf("extension %q owned by both %s and %s", e, prev, l.Name)
			}
			owner[e] = l.Name
		}
	}
}
