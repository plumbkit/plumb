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
// Why WASM for Swift: the pure-Go gotreesitter port cannot reduce an
// implicitly-unwrapped optional type (`var x: T!`) — it emits an ERROR that
// cascades and collapses the enclosing type, dropping it and all its members
// from the outline (pervasive in AppKit/UIKit). The canonical grammar parses it
// cleanly. See docs/internal/treesitter-plan.md.
//
//go:embed swift.wasm
var swiftWasm []byte

// NewSwift returns a WASM-backed Swift extractor. Its fallback is the pure-Go
// gotreesitter Swift extractor (which carries the byte-blanking IUO workaround),
// used only if the wasm runtime cannot initialise.
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
