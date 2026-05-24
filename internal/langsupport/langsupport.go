// Package langsupport is the single source of truth for per-language
// capability in plumb: which structural engine builds the topology "Map"
// (symbols, outlines, ranges) and which LSP adapter, if any, provides the
// semantic "GPS" (definitions, references, types).
//
// It is pure data plus lookups with no dependency on the topology or LSP
// packages, so any layer can consult it without import cycles. The registry is
// immutable after initialisation and therefore safe for concurrent use.
//
// See docs/internal/treesitter-plan.md for the rationale (the per-language
// provider registry and the "tree-sitter is the Map, LSP is the GPS"
// separation of concerns).
package langsupport

import (
	"path/filepath"
	"strings"
)

// StructuralEngine identifies how plumb extracts structural facts (symbols,
// ranges, outlines) for a language. Selection prefers, in order:
// native AST > tree-sitter > regex > none.
type StructuralEngine int

const (
	// EngineNone means no structural extractor: the language is LSP-only, or
	// not yet supported by the topology index.
	EngineNone StructuralEngine = iota
	// EngineNativeAST uses a language's own Go-native parser (go/ast for Go) —
	// preferred where it exists: zero dependency and the most accurate.
	EngineNativeAST
	// EngineTreeSitter uses the pure-Go gotreesitter runtime.
	EngineTreeSitter
	// EngineRegex uses a heuristic line-scan extractor (legacy; being replaced
	// by EngineTreeSitter).
	EngineRegex
)

// Language describes one language's capabilities.
type Language struct {
	// Name is the canonical language name; matches a topology extractor's Language().
	Name string
	// Extensions are the file extensions this language owns: lower-case, dot-prefixed.
	Extensions []string
	// Structural is the engine that builds the topology Map for this language.
	Structural StructuralEngine
	// LSPAdapter is the language-server key in [lsp.<adapter>], or "" when no
	// language server is wired (the structural engine is then the only source).
	LSPAdapter string
}

// registry is the immutable capability table — the single place that encodes
// per-language engine and adapter choices. Iteration order is deterministic.
var registry = []Language{
	{Name: "go", Extensions: []string{".go"}, Structural: EngineNativeAST, LSPAdapter: "gopls"},
	{Name: "python", Extensions: []string{".py"}, Structural: EngineTreeSitter, LSPAdapter: "pyright-langserver"},
	{Name: "typescript", Extensions: []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"}, Structural: EngineRegex, LSPAdapter: ""},
	{Name: "java", Extensions: []string{".java"}, Structural: EngineTreeSitter, LSPAdapter: "jdtls"},
	{Name: "rust", Extensions: []string{".rs"}, Structural: EngineTreeSitter, LSPAdapter: ""},
	{Name: "zig", Extensions: []string{".zig"}, Structural: EngineTreeSitter, LSPAdapter: ""},
	{Name: "kotlin", Extensions: []string{".kt", ".kts"}, Structural: EngineTreeSitter, LSPAdapter: ""},
	{Name: "swift", Extensions: []string{".swift"}, Structural: EngineTreeSitter, LSPAdapter: ""},
	{Name: "bash", Extensions: []string{".sh", ".bash"}, Structural: EngineTreeSitter, LSPAdapter: ""},
	{Name: "hcl", Extensions: []string{".tf", ".tfvars", ".hcl"}, Structural: EngineTreeSitter, LSPAdapter: ""},
	{Name: "sql", Extensions: []string{".sql"}, Structural: EngineTreeSitter, LSPAdapter: ""},
}

// All returns the registry entries. The returned slice must not be mutated.
func All() []Language {
	return registry
}

// ByName returns the Language with the given canonical name, and whether it was found.
func ByName(name string) (Language, bool) {
	for _, l := range registry {
		if l.Name == name {
			return l, true
		}
	}
	return Language{}, false
}

// ByPath returns the Language owning the file's extension, and whether it was found.
func ByPath(path string) (Language, bool) {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return Language{}, false
	}
	for _, l := range registry {
		for _, e := range l.Extensions {
			if e == ext {
				return l, true
			}
		}
	}
	return Language{}, false
}
