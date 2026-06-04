package cli

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestDetectDiagnosticsLang covers the root-marker → (globs, label) mapping that
// decides which files `plumb diagnostics` scans. The key regressions guarded
// here: a Go project that also carries a weak co-located marker (package.json,
// index.html) must still be detected as Go, and a JavaScript project must scan
// .js (not only .ts).
func TestDetectDiagnosticsLang(t *testing.T) {
	tests := []struct {
		name      string
		markers   []string
		wantGlobs []string
		wantLabel string
	}{
		{"empty dir falls back to Go", nil, []string{"*.go"}, "Go"},
		{"go.mod is Go", []string{"go.mod"}, []string{"*.go"}, "Go"},
		{
			"go.mod wins over a co-located package.json",
			[]string{"go.mod", "package.json"},
			[]string{"*.go"},
			"Go",
		},
		{
			"go.mod wins over a co-located index.html",
			[]string{"go.mod", "index.html"},
			[]string{"*.go"},
			"Go",
		},
		{
			"package.json scans JS and TS extensions",
			[]string{"package.json"},
			[]string{"*.ts", "*.tsx", "*.js", "*.jsx", "*.mjs", "*.cjs"},
			"TypeScript/JavaScript",
		},
		{"tsconfig.json scans ts and tsx", []string{"tsconfig.json"}, []string{"*.ts", "*.tsx"}, "TypeScript"},
		{"Cargo.toml is Rust", []string{"Cargo.toml"}, []string{"*.rs"}, "Rust"},
		{"Package.swift is Swift", []string{"Package.swift"}, []string{"*.swift"}, "Swift"},
		{"index.html is HTML", []string{"index.html"}, []string{"*.html", "*.htm"}, "HTML"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, m := range tt.markers {
				if err := os.WriteFile(filepath.Join(dir, m), []byte("x"), 0o644); err != nil {
					t.Fatalf("writing marker %s: %v", m, err)
				}
			}
			globs, label := detectDiagnosticsLang(dir)
			if !slices.Equal(globs, tt.wantGlobs) {
				t.Errorf("globs = %v, want %v", globs, tt.wantGlobs)
			}
			if label != tt.wantLabel {
				t.Errorf("label = %q, want %q", label, tt.wantLabel)
			}
		})
	}
}
