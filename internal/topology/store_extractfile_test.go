package topology

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// linkedExtractor draws one containment edge between two nodes whose spans do
// not nest — the shape of a Rust method and the type its `impl` block names.
type linkedExtractor struct{}

func (linkedExtractor) Language() string     { return "toy" }
func (linkedExtractor) Extensions() []string { return []string{".toy"} }
func (linkedExtractor) Extract(_ context.Context, relPath string, _ []byte) ([]Node, []Edge, error) {
	return []Node{
			{Kind: KindType, Name: "Foo", Qualified: "Foo", StartLine: 1, EndLine: 1, Language: "toy", Path: relPath},
			{Kind: KindMethod, Name: "run", Qualified: "run", StartLine: 3, EndLine: 3, Language: "toy", Path: relPath},
		},
		[]Edge{{FromID: 0, ToID: 1, Kind: EdgeContains, Confidence: 0.8, Source: "heuristic"}},
		nil
}

// TestStore_ExtractFileGraphReturnsTheExtractorsEdges: the symbol-edit fallback
// resolves Type/method through the containment edge where no span says whose
// member a method is, so the re-parse has to hand the edges over — as indices
// into the nodes it returns, since nothing is persisted and no ID is assigned.
// ExtractFile stays the nodes of the same parse.
func TestStore_ExtractFileGraphReturnsTheExtractorsEdges(t *testing.T) {
	ws := t.TempDir()
	path := filepath.Join(ws, "a.toy")
	if err := os.WriteFile(path, []byte("type Foo\n\nmethod run\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024}, []Extractor{linkedExtractor{}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	nodes, edges, err := s.ExtractFileGraph(context.Background(), path)
	if err != nil {
		t.Fatalf("ExtractFileGraph: %v", err)
	}
	if len(nodes) != 2 || len(edges) != 1 {
		t.Fatalf("got %d nodes and %d edges, want 2 and 1", len(nodes), len(edges))
	}
	if e := edges[0]; e.Kind != EdgeContains || nodes[e.FromID].Name != "Foo" || nodes[e.ToID].Name != "run" {
		t.Errorf("edge %+v does not link Foo to run by index into the returned nodes", e)
	}

	plain, err := s.ExtractFile(context.Background(), path)
	if err != nil {
		t.Fatalf("ExtractFile: %v", err)
	}
	if !slices.Equal(plain, nodes) {
		t.Errorf("ExtractFile = %+v, want the same nodes as ExtractFileGraph %+v", plain, nodes)
	}
}
