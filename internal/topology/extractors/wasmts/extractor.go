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

	// mu guards the lazily-built runtime. Not a sync.Once: a parse terminated by
	// its context leaves wazero's module closed, so the runtime has to be
	// discardable and rebuildable rather than built exactly once.
	mu       sync.Mutex
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

// ensure returns the extractor's runtime, building it on first use and again
// after a discard. The build deliberately runs on a ctx stripped of the
// caller's deadline: compiling the bundle is one-off setup, not the per-file
// parse the deadline is meant to bound, and a runtime built under a nearly
// expired context would be born closed.
func (e *Extractor) ensure(ctx context.Context) (*runtime, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rt == nil && e.initErr == nil {
		e.rt, e.initErr = newRuntime(context.WithoutCancel(ctx), e.wasm, e.exports)
	}
	return e.rt, e.initErr
}

// discard drops the extractor's reference to a runtime whose parse overran its
// deadline, so the next Extract builds a fresh one instead of queueing behind
// the stuck parse's lock. Guarded by identity so a concurrent caller that has
// already rebuilt is not undone.
//
// It deliberately does NOT close the runtime. The abandoned goroutine is still
// executing inside that wasm module, and without the interruptible mode there
// are no check points at which it would notice a close — tearing down the
// module would free linear memory out from under a live parse. Dropping the
// reference is enough: the stuck goroutine holds the last one, so the runtime
// becomes garbage as soon as its parse finally returns.
func (e *Extractor) discard(stuck *runtime) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rt != stuck {
		return
	}
	e.rt, e.initErr = nil, nil
}

// Extract parses src and returns the grammar's symbols and edges. Containment is
// lexical and certain (1.0/extractor); intra-file call edges are name-resolved
// heuristics (0.8). On any wasm-init or parse fault it degrades to the fallback
// extractor.
//
// The parse runs on its own goroutine so a ctx deadline can end the WAIT even
// though it cannot end the parse (see newRuntime for why the interruptible
// wazero mode is not worth its cost). One parse holds the runtime's lock for its
// whole duration, so a parse that overruns would otherwise serialise every later
// file behind it — each then burning its own full timeout. Discarding the
// runtime is what prevents that: the overrunning parse keeps the old one to
// itself and the next file builds a fresh one.
func (e *Extractor) Extract(ctx context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	// A context that is already dead starts no parse — matching the treesitter
	// envelope's contract (an expired budget starts no parse). Without this, a
	// cancelled call spawns a parse doomed to be abandoned at the select below,
	// discarding a warm runtime and leaking the parse goroutine for nothing;
	// worse, a fast parse can win the select against the already-closed
	// ctx.Done() and return a result where the caller was promised ctx.Err().
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	rt, initErr := e.ensure(ctx)
	if initErr != nil || rt == nil {
		e.warnOnce.Do(func() {
			slog.Warn("wasmts: tree-sitter wasm unavailable; using fallback", "lang", e.langName, "err", initErr)
		})
		return e.fallback.Extract(ctx, relPath, src)
	}

	type parsed struct {
		nodes []topology.Node
		edges []topology.Edge
		err   error
	}
	// Buffered: an abandoned parse must be able to send and exit rather than
	// block forever on a receiver that has moved on.
	done := make(chan parsed, 1)
	go func() {
		var nodes []topology.Node
		var edges []topology.Edge
		err := rt.parse(ctx, rt.langs[e.primary], src, func(root node) {
			nodes, edges = e.build(root, relPath, src, newLineMap(src))
		})
		done <- parsed{nodes: nodes, edges: edges, err: err}
	}()

	var res parsed
	select {
	case res = <-done:
	case <-ctx.Done():
		slog.Warn("wasmts: parse abandoned at the deadline; discarding the runtime",
			"lang", e.langName, "path", relPath, "bytes", len(src))
		e.discard(rt)
		return nil, nil, ctx.Err()
	}

	if res.err != nil {
		e.warnOnce.Do(func() {
			slog.Warn("wasmts: wasm parse fault; using fallback", "lang", e.langName, "path", relPath, "err", res.err)
		})
		return e.fallback.Extract(ctx, relPath, src)
	}
	return res.nodes, res.edges, nil
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
