package wasmts

import (
	_ "embed"

	"github.com/plumbkit/plumb/internal/topology"
	"github.com/plumbkit/plumb/internal/topology/extractors/treesitter"
)

// swift.wasm bundles the canonical tree-sitter runtime + the canonical
// alex-pinkus/tree-sitter-swift grammar (0.7.1, ABI14) and its C external
// scanner, compiled to wasm32-wasi by csrc/build-swift.sh. It is committed so
// building plumb needs only Go + wazero (no C toolchain). See csrc/NOTICE.md.
//
// Why WASM for Swift: the pure-Go gotreesitter port still diverges from the
// canonical grammar on real Swift code, and an ERROR there drops the enclosing
// type and its members from the outline. It first collapsed on implicitly-
// unwrapped optionals (`var x: T!`, fixed in v0.47). Since v0.54 it fails on a
// `#` token (`#if`, `#warning`, `#Preview`, `#expect`) that follows a statement;
// TestSwift_HashTokenAfterStatement_GotreesitterStillBroken is the tripwire. The
// canonical grammar parses both cleanly. The retirement gate is PLAN-1.
//
//go:embed swift.wasm
var swiftWasm []byte

// NewSwift returns a WASM-backed Swift extractor. Its fallback is the pure-Go
// gotreesitter Swift extractor. It is used when the wasm runtime cannot
// initialise, and per file when a wasm parse faults (Extract) — which logs
// only the first fault, so later fallbacks are silent.
func NewSwift() *Extractor {
	return &Extractor{
		langName: "swift", exts: []string{".swift"},
		wasm: swiftWasm, exports: []string{"tree_sitter_swift"}, primary: "tree_sitter_swift",
		build: buildSwift, fallback: treesitter.NewSwift(),
	}
}

// buildSwift walks a parsed Swift tree (canonical grammar) into topology nodes
// and edges, matching the gotreesitter Swift extractor's output shape.
func buildSwift(root node, relPath string, src []byte, lines *lineMap) ([]topology.Node, []topology.Edge) {
	w := &swiftWalk{src: src, path: relPath, lines: lines, funcIdx: map[string]int64{}, conf: map[int64]string{}}
	w.walk(root, -1, false, false)
	w.callEdges(root)
	return w.nodes, w.edges
}
