package wasmts

import (
	"context"
	"log/slog"
	"sort"
	"sync"

	"github.com/golimpio/plumb/internal/topology"
	tsregex "github.com/golimpio/plumb/internal/topology/extractors/typescript"
)

// grammar selects which embedded tree-sitter grammar an extractor drives.
type grammar int

const (
	grammarTS grammar = iota
	grammarTSX
)

// Extractor extracts TypeScript-family symbols using the canonical
// tree-sitter-typescript grammar compiled to WASM and driven by wazero. Unlike
// the pure-Go gotreesitter runtime, the canonical grammar parses typed arrow
// parameters in TSX without cascading ERROR nodes (the defect that kept
// .tsx/.jsx on the regex extractor — see docs/internal/treesitter-plan.md).
//
// Two instances are constructed: NewTypeScript (.ts, typescript grammar) and
// NewTSX (.tsx/.jsx, tsx grammar). Each owns its own lazily-initialised wazero
// runtime, built on first Extract and reused for the daemon's lifetime, so a
// workspace that never sees .tsx files never pays for the tsx runtime.
//
// Robustness: if the WASM runtime cannot initialise (it is pure-Go and
// cross-platform, so this is not expected), Extract degrades to the legacy
// regex extractor and logs once, rather than dropping the language entirely.
//
// Concurrency: safe for concurrent Extract calls; the underlying runtime
// serialises parses through one wasm module (see runtime).
type Extractor struct {
	langName string
	exts     []string
	grammar  grammar
	fallback topology.Extractor

	once     sync.Once
	rt       *runtime
	initErr  error
	warnOnce sync.Once
}

// NewTypeScript returns a WASM-backed extractor for TypeScript (.ts).
func NewTypeScript() *Extractor {
	return &Extractor{langName: "typescript", exts: []string{".ts"}, grammar: grammarTS, fallback: tsregex.New()}
}

// NewTSX returns a WASM-backed extractor for TSX/JSX (.tsx/.jsx). Its nodes are
// labelled language "typescript" (not "tsx") so .ts and .tsx symbols search
// together under one language, matching the langsupport tsx-alias convention.
func NewTSX() *Extractor {
	return &Extractor{langName: "typescript", exts: []string{".tsx", ".jsx"}, grammar: grammarTSX, fallback: tsregex.New()}
}

func (e *Extractor) Language() string     { return e.langName }
func (e *Extractor) Extensions() []string { return e.exts }

func (e *Extractor) ensure(ctx context.Context) *runtime {
	e.once.Do(func() { e.rt, e.initErr = newRuntime(ctx) })
	return e.rt
}

func (e *Extractor) langPtr() uint64 {
	if e.grammar == grammarTSX {
		return e.rt.tsxLang
	}
	return e.rt.tsLang
}

// Extract parses src and returns top-level functions, classes with their
// methods, interfaces (→ KindType) with their method signatures, type aliases
// and enums (→ KindType) with members (→ KindConstant), top-level
// constants/variables, imports, and describe/it/test blocks (→ KindTest);
// namespace bodies are descended into. Containment is lexical and certain
// (1.0/extractor); intra-file call edges are name-resolved heuristics (0.8).
func (e *Extractor) Extract(ctx context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	rt := e.ensure(ctx)
	if e.initErr != nil || rt == nil {
		e.warnOnce.Do(func() {
			slog.Warn("wasmts: tree-sitter wasm unavailable; using regex fallback", "err", e.initErr)
		})
		return e.fallback.Extract(ctx, relPath, src)
	}

	w := &walk{src: src, path: relPath, lang: e.langName, lines: newLineMap(src), funcIdx: map[string]int64{}}
	err := rt.parse(ctx, e.langPtr(), src, func(root node) {
		for _, c := range root.children() {
			w.dispatch(c)
		}
		w.scanTests(root)
		w.callEdges(root)
	})
	if err != nil {
		e.warnOnce.Do(func() {
			slog.Warn("wasmts: wasm parse fault; using regex fallback", "path", relPath, "err", err)
		})
		return e.fallback.Extract(ctx, relPath, src)
	}
	return w.nodes, w.edges, nil
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
