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
	// PreferStructuralOutline makes outline-style tools (file_outline,
	// list_symbols, find_symbol) consult the topology Map BEFORE the language
	// server. Set for markup languages whose LSP documentSymbol is unusable as an
	// outline (vscode-html emits a node per tag AND attribute — ~1220 for one
	// page vs the Map's ~54 clean landmarks). The LSP stays the source for
	// hover/diagnostics; only the outline prefers the Map.
	PreferStructuralOutline bool
	// MaxParseBytes is a per-grammar source-size ceiling for structural
	// extraction, tighter than the global [topology].max_file_size_bytes. Set
	// only for GLR-heavy markup grammars (Markdown ~200 nodes/byte, HTML per
	// tag/attribute, YAML) where one few-hundred-KB file drives a pathological
	// parse for little outline value; a file above the cap is indexed with zero
	// symbols rather than parsed. 0 means no per-grammar cap (the global limit is
	// the only bound). This is a grammar property, not a user preference, so it is
	// not config-exposed.
	MaxParseBytes int64
}

// registry is the immutable capability table — the single place that encodes
// per-language engine and adapter choices. Iteration order is deterministic.
var registry = []Language{
	{Name: "go", Extensions: []string{".go"}, Structural: EngineNativeAST, LSPAdapter: "gopls"},
	{Name: "python", Extensions: []string{".py"}, Structural: EngineTreeSitter, LSPAdapter: "pyright-langserver"},
	{Name: "typescript", Extensions: []string{".ts"}, Structural: EngineTreeSitter, LSPAdapter: "typescript-language-server"},
	{Name: "tsx", Extensions: []string{".tsx", ".jsx"}, Structural: EngineTreeSitter, LSPAdapter: "typescript-language-server"},
	{Name: "javascript", Extensions: []string{".js", ".mjs", ".cjs"}, Structural: EngineTreeSitter, LSPAdapter: "typescript-language-server"},
	{Name: "java", Extensions: []string{".java"}, Structural: EngineTreeSitter, LSPAdapter: "jdtls"},
	{Name: "rust", Extensions: []string{".rs"}, Structural: EngineTreeSitter, LSPAdapter: "rust-analyzer"},
	{Name: "zig", Extensions: []string{".zig"}, Structural: EngineTreeSitter, LSPAdapter: "zls"},
	{Name: "kotlin", Extensions: []string{".kt", ".kts"}, Structural: EngineTreeSitter, LSPAdapter: "kotlin-language-server"},
	{Name: "swift", Extensions: []string{".swift"}, Structural: EngineTreeSitter, LSPAdapter: "sourcekit-lsp"},
	{Name: "bash", Extensions: []string{".sh", ".bash"}, Structural: EngineTreeSitter, LSPAdapter: ""},
	{Name: "hcl", Extensions: []string{".tf", ".tfvars", ".hcl"}, Structural: EngineTreeSitter, LSPAdapter: ""},
	{Name: "sql", Extensions: []string{".sql"}, Structural: EngineTreeSitter, LSPAdapter: ""},
	{Name: "dockerfile", Extensions: []string{"dockerfile", "containerfile"}, Structural: EngineTreeSitter, LSPAdapter: ""},
	{Name: "toml", Extensions: []string{".toml"}, Structural: EngineTreeSitter, LSPAdapter: ""},
	{Name: "yaml", Extensions: []string{".yaml", ".yml"}, Structural: EngineTreeSitter, LSPAdapter: "", MaxParseBytes: 256 << 10},
	{Name: "markdown", Extensions: []string{".md", ".markdown"}, Structural: EngineTreeSitter, LSPAdapter: "", PreferStructuralOutline: true, MaxParseBytes: 256 << 10},
	{Name: "html", Extensions: []string{".html", ".htm"}, Structural: EngineTreeSitter, LSPAdapter: "vscode-html-language-server", PreferStructuralOutline: true, MaxParseBytes: 256 << 10},
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

// ByPath returns the Language owning the file, and whether it was found. An
// extension pattern (dot-prefixed, e.g. ".go") matches the file extension; a
// bare filename pattern (e.g. "dockerfile") matches the basename exactly or as
// a dotted prefix/suffix ("Dockerfile", "Dockerfile.prod", "prod.dockerfile"),
// so extensionless files are recognised.
func ByPath(path string) (Language, bool) {
	ext := strings.ToLower(filepath.Ext(path))
	base := strings.ToLower(filepath.Base(path))
	for _, l := range registry {
		for _, e := range l.Extensions {
			if MatchExtPattern(e, ext, base) {
				return l, true
			}
		}
	}
	return Language{}, false
}

// MatchExtPattern reports whether a registry-style pattern matches a file's
// extension or basename. Dot-prefixed patterns (".go") match the extension; bare
// patterns ("dockerfile") match the basename exactly or as a dotted
// prefix/suffix ("Dockerfile", "Dockerfile.prod", "prod.dockerfile"). It is the
// single source of truth for this rule, shared with the topology extractor
// matcher so the two cannot drift.
func MatchExtPattern(pat, ext, base string) bool {
	if strings.HasPrefix(pat, ".") {
		return pat == ext
	}
	return base == pat || strings.HasPrefix(base, pat+".") || strings.HasSuffix(base, "."+pat)
}
