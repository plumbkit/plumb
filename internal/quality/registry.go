package quality

// registry.go answers one question about every string a user can put in
// [quality] analysers: what is it, and can plumb run it?
//
// Before this table the answer was a single switch case in cli.buildAnalysers,
// and every other spelling — "ruff", "eslint", a typo, an absolute path — was
// dropped mid-loop with no error, no log, and no mark in the Settings pane. A
// user who configured ruff, restarted the daemon and saw no Python findings had
// nothing anywhere to distinguish "plumb does not support this" from "the binary
// is not installed" from "you spelled it wrong".
//
// The Implemented flag is the point of the table, not an implementation detail.
// A row with Implemented=false grants nothing and costs nothing at runtime; it
// exists so that "plumb knows this tool and has no adapter for it" is a fact the
// codebase can state, rather than an absence the user has to infer from silence.
// It is the same device langsupport uses for a language it recognises but does
// not index, and for the same reason.
//
// Concurrency: the table is immutable package data, read-only after init.

import (
	"path/filepath"
	"strings"
)

// Ecosystem selects the fallback directories LookBinary searches after PATH.
//
// It exists because the daemon does not run with the user's interactive PATH:
// it inherits the environment of whichever `plumb serve` proxy spawned it,
// captured when that agent session started. Each language's package manager
// installs into a directory that environment routinely lacks, and which one
// differs per ecosystem — so the fallback has to be per tool, not one global
// list.
type Ecosystem int

const (
	// EcosystemNone searches PATH only.
	EcosystemNone Ecosystem = iota
	// EcosystemGo adds $GOBIN, the first $GOPATH element's bin, and ~/go/bin —
	// where `go install` puts a binary.
	EcosystemGo
	// EcosystemPython adds $VIRTUAL_ENV/bin and ~/.local/bin — where a venv and
	// `pip install --user` / `uv tool install` respectively put a binary.
	EcosystemPython
	// EcosystemNode adds the npm global prefix's bin, then ~/.local/bin.
	EcosystemNode
)

// Tool describes one analyser plumb recognises by name.
type Tool struct {
	// Name is the spelling used in [quality] analysers. It is the identity: the
	// binary, the language and the adapter all hang off it.
	Name string
	// Language is the canonical langsupport language name this tool analyses.
	// TestRegistry_LanguagesExistInLangsupport pins the two tables together, so
	// the language shown beside an analyser in the Settings pane is a fact
	// rather than a label.
	Language string
	// Extensions are the file extensions the tool analyses: lower-case,
	// dot-prefixed. A subset of the language's — a Python type checker and a
	// Python linter own the same files, but a Go vet tool owns .go and not
	// .go.tmpl.
	Extensions []string
	// Binary is the executable basename to resolve. Usually Name, but not
	// always: clippy ships as `cargo-clippy`.
	Binary string
	// Ecosystem selects LookBinary's fallback directories.
	Ecosystem Ecosystem
	// Implemented reports whether plumb has an Analyser adapter for this tool.
	// False means recognised-but-unsupported: the name is understood, the row
	// renders as such, and nothing runs.
	Implemented bool
}

// registry is the immutable capability table. Ordering is deterministic and
// groups by language so a reader can see the coverage per ecosystem.
//
// The unimplemented rows are the tools a user of that language is actually
// likely to type. They are deliberately curated rather than exhaustive: a name
// nobody reaches for adds a row to maintain and buys no better message than the
// unrecognised-name path already gives.
var registry = []Tool{
	// --- Go
	{
		Name: "golangci-lint", Language: "go", Extensions: []string{".go"},
		Binary: "golangci-lint", Ecosystem: EcosystemGo, Implemented: true,
	},
	{
		Name: "staticcheck", Language: "go", Extensions: []string{".go"},
		Binary: "staticcheck", Ecosystem: EcosystemGo,
	},

	// --- Python
	{
		Name: "ruff", Language: "python", Extensions: []string{".py", ".pyi"},
		Binary: "ruff", Ecosystem: EcosystemPython, Implemented: true,
	},
	{
		Name: "mypy", Language: "python", Extensions: []string{".py", ".pyi"},
		Binary: "mypy", Ecosystem: EcosystemPython,
	},
	{
		Name: "pylint", Language: "python", Extensions: []string{".py"},
		Binary: "pylint", Ecosystem: EcosystemPython,
	},

	// --- TypeScript / JavaScript. The registry names the language whose sources
	// the tool is most often pointed at; Extensions carry the rest, because
	// Supports is decided by extension and never by the language name.
	{
		Name: "eslint", Language: "typescript",
		Extensions: []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"},
		Binary:     "eslint", Ecosystem: EcosystemNode,
	},
	{
		Name: "biome", Language: "typescript",
		Extensions: []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"},
		Binary:     "biome", Ecosystem: EcosystemNode,
	},
	{
		Name: "oxlint", Language: "typescript",
		Extensions: []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"},
		Binary:     "oxlint", Ecosystem: EcosystemNode,
	},

	// --- Rust. clippy is invoked as `cargo clippy` but ships as its own
	// binary, which is what has to be on PATH for the subcommand to resolve.
	{
		Name: "clippy", Language: "rust", Extensions: []string{".rs"},
		Binary: "cargo-clippy",
	},

	// --- JVM
	{Name: "ktlint", Language: "kotlin", Extensions: []string{".kt", ".kts"}, Binary: "ktlint"},
	{Name: "detekt", Language: "kotlin", Extensions: []string{".kt", ".kts"}, Binary: "detekt"},
	{Name: "checkstyle", Language: "java", Extensions: []string{".java"}, Binary: "checkstyle"},

	// --- Apple
	{Name: "swiftlint", Language: "swift", Extensions: []string{".swift"}, Binary: "swiftlint"},

	// --- Shell and systems
	{Name: "shellcheck", Language: "bash", Extensions: []string{".sh", ".bash"}, Binary: "shellcheck"},
	{
		Name: "clang-tidy", Language: "c",
		Extensions: []string{".c", ".h", ".cc", ".cpp", ".cxx", ".hh", ".hpp", ".hxx"},
		Binary:     "clang-tidy",
	},

	// --- Scripting
	{Name: "rubocop", Language: "ruby", Extensions: []string{".rb", ".rake", ".gemspec"}, Binary: "rubocop"},
	{Name: "phpstan", Language: "php", Extensions: []string{".php"}, Binary: "phpstan"},
	{Name: "luacheck", Language: "lua", Extensions: []string{".lua"}, Binary: "luacheck"},

	// --- Config, infrastructure and markup
	{Name: "hadolint", Language: "dockerfile", Extensions: []string{"dockerfile"}, Binary: "hadolint"},
	{Name: "tflint", Language: "hcl", Extensions: []string{".tf", ".tfvars", ".hcl"}, Binary: "tflint"},
	{Name: "sqlfluff", Language: "sql", Extensions: []string{".sql"}, Binary: "sqlfluff", Ecosystem: EcosystemPython},
	{
		Name: "yamllint", Language: "yaml", Extensions: []string{".yaml", ".yml"},
		Binary: "yamllint", Ecosystem: EcosystemPython,
	},
	{
		Name: "markdownlint", Language: "markdown", Extensions: []string{".md", ".markdown"},
		Binary: "markdownlint", Ecosystem: EcosystemNode,
	},
	{
		Name: "stylelint", Language: "css", Extensions: []string{".css", ".scss"},
		Binary: "stylelint", Ecosystem: EcosystemNode,
	},
	{Name: "taplo", Language: "toml", Extensions: []string{".toml"}, Binary: "taplo"},
}

// Tools returns the registry entries. The returned slice must not be mutated.
func Tools() []Tool {
	return registry
}

// ToolByName returns the Tool with the given config spelling, and whether it was
// found. The match is exact: an analyser name is an identifier, not a search
// term, and accepting near-misses would turn a typo into a silently different
// tool.
func ToolByName(name string) (Tool, bool) {
	for _, t := range registry {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// ImplementedNames returns the names of every tool plumb has an adapter for, in
// registry order. cli.buildAnalysers is pinned against it, so a row marked
// Implemented with no constructor — or a constructor with no row — fails a test
// rather than becoming a name that resolves in one place and not the other.
func ImplementedNames() []string {
	out := make([]string, 0, len(registry))
	for _, t := range registry {
		if t.Implemented {
			out = append(out, t.Name)
		}
	}
	return out
}

// SupportsPath reports whether the tool analyses the file at path, by extension.
// Shared by every adapter's Supports method so the registry stays the single
// statement of which files a tool owns.
func (t Tool) SupportsPath(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	base := strings.ToLower(filepath.Base(path))
	for _, e := range t.Extensions {
		if strings.HasPrefix(e, ".") {
			if e == ext {
				return true
			}
			continue
		}
		// A bare pattern names an extensionless file (dockerfile), matched the
		// way langsupport.MatchExtPattern matches one.
		if base == e || strings.HasPrefix(base, e+".") || strings.HasSuffix(base, "."+e) {
			return true
		}
	}
	return false
}
