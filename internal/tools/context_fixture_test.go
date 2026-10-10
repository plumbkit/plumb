package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/topology"
	goext "github.com/plumbkit/plumb/internal/topology/extractors/golang"
	"github.com/plumbkit/plumb/internal/topology/extractors/treesitter"
)

// shopFixtureFiles is how many files of testdata/contextpack/shop the extractors
// below index: the Go, Python and Markdown sources. The Svelte file and go.mod
// are recorded by the indexer without symbols.
const shopFixtureFiles = 12

// copyShopFixture copies the contextpack "shop" fixture into a fresh temporary
// workspace and returns its root. The copy keeps each test's index and any file
// it mutates out of the shared testdata.
func copyShopFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS(filepath.Join("testdata", "contextpack", "shop"))); err != nil {
		t.Fatalf("copy the shop fixture: %v", err)
	}
	return root
}

// openShopStore indexes the shop fixture rooted at root.
func openShopStore(t *testing.T, root string) *topology.Store {
	t.Helper()
	return openContextStore(t, root, shopFixtureFiles)
}

// openContextStore indexes root with the real Go, Python and Markdown
// extractors and waits until wantFiles files are in the index. Indexing is real
// on purpose: ambiguity and receiver-form equivalence are properties of what
// the extractors emit, which a hand-built index would only restate.
func openContextStore(t *testing.T, root string, wantFiles int) *topology.Store {
	t.Helper()
	s, err := topology.Open(root, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New(), treesitter.NewPython(), treesitter.NewMarkdown()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if st := s.Status(); st.IndexedFiles >= wantFiles && st.IndexerState != "running" {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%d files were not indexed in time: %+v", wantFiles, s.Status())
	return nil
}

// shopTool is a context_for_task tool wired the way the daemon wires it, over a
// real store, pinned to root with a real boundary guard.
type shopTool struct {
	root      string // the workspace as the agent sees it
	collector *ContextCollector
	tool      *ContextForTask
}

func newShopTool(t *testing.T, store *topology.Store, root string) shopTool {
	t.Helper()
	collector := NewContextCollector(func() *topology.Store { return store }).
		WithWorkspace(func(context.Context) string { return root }).
		WithBoundary(testBoundaryGuard(root))
	return shopTool{root: root, collector: collector, tool: NewContextForTask(collector)}
}

// newShop copies, indexes and wires the fixture in one step.
func newShop(t *testing.T) shopTool {
	t.Helper()
	root := copyShopFixture(t)
	return newShopTool(t, openShopStore(t, root), root)
}

// run calls the tool with args and returns its text or its error.
func (s shopTool) run(t *testing.T, args map[string]any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return s.tool.Execute(context.Background(), raw)
}

// collect validates args the way Execute does and returns the structured pack.
func (s shopTool) collect(t *testing.T, args map[string]any) contextPack {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	req, err := parseContextRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := req.normalise(); err != nil {
		t.Fatal(err)
	}
	pack, err := s.collector.Collect(context.Background(), req)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return pack
}
