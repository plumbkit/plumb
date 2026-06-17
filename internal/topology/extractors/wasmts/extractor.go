package wasmts

import (
	"context"
	_ "embed"
	"log/slog"
	"sort"
	"sync"

	"github.com/plumbkit/plumb/internal/topology"
	tsregex "github.com/plumbkit/plumb/internal/topology/extractors/typescript"
)

// ts.wasm bundles the canonical tree-sitter runtime + tree-sitter-typescript
// (typescript + tsx) grammars, compiled to wasm32-wasi by csrc/build.sh. It is
// committed so building plumb needs only Go + wazero (no C toolchain). See
// csrc/NOTICE.md for provenance and regeneration.
//
//go:embed ts.wasm
var tsWasm []byte

// builder turns a parsed root node into topology nodes/edges. Each grammar
// (TypeScript, Swift) supplies its own; the Extractor is otherwise grammar-
// agnostic.
type builder func(root node, relPath string, src []byte, lines *lineMap) ([]topology.Node, []topology.Edge)

// Extractor extracts symbols by parsing with a canonical tree-sitter grammar
// compiled to WASM and driven by wazero. Unlike the pure-Go gotreesitter
// runtime, the canonical grammars parse constructs that defeat the port —
// typed arrow parameters in TSX, implicitly-unwrapped optional types in Swift —
// without cascading ERROR nodes.
//
// Each instance owns its own lazily-initialised wazero runtime, built on first
// Extract and reused for the daemon's lifetime, so a workspace that never sees a
// grammar's files never pays for its runtime.
//
// Robustness: if the WASM runtime cannot initialise (it is pure-Go and
// cross-platform, so this is not expected), Extract degrades to a fallback
// extractor and logs once, rather than dropping the language entirely.
//
// Concurrency: safe for concurrent Extract calls; the underlying runtime
// serialises parses through one wasm module (see runtime).
type Extractor struct {
	langName string
	exts     []string
	wasm     []byte
	exports  []string // grammar exports to load from the bundle
	primary  string   // the export this extractor parses with
	build    builder
	fallback topology.Extractor

	once     sync.Once
	rt       *runtime
	initErr  error
	warnOnce sync.Once
}

// tsExports are the two grammars in ts.wasm.
var tsExports = []string{"tree_sitter_typescript", "tree_sitter_tsx"}

// NewTypeScript returns a WASM-backed extractor for TypeScript (.ts).
func NewTypeScript() *Extractor {
	return &Extractor{
		langName: "typescript", exts: []string{".ts"},
		wasm: tsWasm, exports: tsExports, primary: "tree_sitter_typescript",
		build: buildTS("typescript"), fallback: tsregex.New(),
	}
}

// NewTSX returns a WASM-backed extractor for TSX/JSX (.tsx/.jsx). Its nodes are
// labelled language "typescript" (not "tsx") so .ts and .tsx symbols search
// together under one language, matching the langsupport tsx-alias convention.
func NewTSX() *Extractor {
	return &Extractor{
		langName: "typescript", exts: []string{".tsx", ".jsx"},
		wasm: tsWasm, exports: tsExports, primary: "tree_sitter_tsx",
		build: buildTS("typescript"), fallback: tsregex.New(),
	}
}

// buildTS returns the TypeScript-family builder labelling nodes with lang.
func buildTS(lang string) builder {
	return func(root node, relPath string, src []byte, lines *lineMap) ([]topology.Node, []topology.Edge) {
		w := &walk{src: src, path: relPath, lang: lang, lines: lines, funcIdx: map[string]int64{}}
		for _, c := range root.children() {
			w.dispatch(c)
		}
		w.scanTests(root)
		w.callEdges(root)
		return w.nodes, w.edges
	}
}

func (e *Extractor) Language() string     { return e.langName }
func (e *Extractor) Extensions() []string { return e.exts }

func (e *Extractor) ensure(ctx context.Context) *runtime {
	e.once.Do(func() { e.rt, e.initErr = newRuntime(ctx, e.wasm, e.exports) })
	return e.rt
}

// Extract parses src and returns the grammar's symbols and edges. Containment is
// lexical and certain (1.0/extractor); intra-file call edges are name-resolved
// heuristics (0.8). On any wasm-init or parse fault it degrades to the fallback
// extractor.
func (e *Extractor) Extract(ctx context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	rt := e.ensure(ctx)
	if e.initErr != nil || rt == nil {
		e.warnOnce.Do(func() {
			slog.Warn("wasmts: tree-sitter wasm unavailable; using fallback", "lang", e.langName, "err", e.initErr)
		})
		return e.fallback.Extract(ctx, relPath, src)
	}

	var nodes []topology.Node
	var edges []topology.Edge
	err := rt.parse(ctx, rt.langs[e.primary], src, func(root node) {
		nodes, edges = e.build(root, relPath, src, newLineMap(src))
	})
	if err != nil {
		e.warnOnce.Do(func() {
			slog.Warn("wasmts: wasm parse fault; using fallback", "lang", e.langName, "path", relPath, "err", err)
		})
		return e.fallback.Extract(ctx, relPath, src)
	}
	return nodes, edges, nil
}

// lineMap converts a byte offset to a 1-based line number, matching tree-sitter's
// row+1. It precomputes newline offsets once per file for O(log n) lookups.
type lineMap struct {
	nl []int // ascending byte offsets of '\n'
}

func newLineMap(src []byte) *lineMap {
	var nl []int
	for i, b := range src {
		if b == '\n' {
			nl = append(nl, i)
		}
	}
	return &lineMap{nl: nl}
}

func (m *lineMap) at(byteOff int) int {
	// line = (newlines strictly before byteOff) + 1
	return sort.Search(len(m.nl), func(i int) bool { return m.nl[i] >= byteOff }) + 1
}

// col returns the 0-based byte column of byteOff: its distance from the start of
// its line. The grammar lacks point exports, so columns are derived here from the
// same newline table that backs line lookup — cheap and pure-Go (byte columns,
// not rune columns, matching the byte-offset span).
func (m *lineMap) col(byteOff int) int {
	i := sort.Search(len(m.nl), func(i int) bool { return m.nl[i] >= byteOff })
	if i == 0 {
		return byteOff
	}
	return byteOff - m.nl[i-1] - 1
}
