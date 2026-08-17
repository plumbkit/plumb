package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/topology"
	"github.com/plumbkit/plumb/internal/topology/extractors/treesitter"
)

// TestDocCommentStartColumnAgreement pins PLAN-288: the topology path
// (topologyDocCommentStart, via byteOffsetToPosition on the extractor's doc
// span) and the line-scan fallback (docCommentStart) must return the SAME edit
// position for an indented member — column 0 of the comment's first line, not
// the comment's own column. Pre-fix the topology path returned column 4 (the
// '#' of an indented method's comment), so replace_symbol_body with
// include_doc_comment started the edit range at the wrong column and
// double-indented replacement content carrying its own indentation.
func TestDocCommentStartColumnAgreement(t *testing.T) {
	src := "class Widget:\n    # Does the thing.\n    def run(self):\n        return 1\n"
	ws := t.TempDir()
	path := filepath.Join(ws, "widget.py")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{treesitter.NewPython()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Line 2 (0-based) is `    def run(self):` — the indented member.
	lineScan := docCommentStart(path, protocol.Position{Line: 2})
	topo, ok := topologyDocCommentStart(context.Background(),
		func() *topology.Store { return store }, "file://"+path, "run")
	if !ok {
		t.Fatal("topology path should resolve a doc span for the indented method")
	}
	if lineScan != topo {
		t.Errorf("doc-comment start paths disagree: line-scan %+v, topology %+v", lineScan, topo)
	}
	if topo.Character != 0 {
		t.Errorf("doc-comment start column = %d, want 0 (start of the comment's line, not the comment)", topo.Character)
	}
}
