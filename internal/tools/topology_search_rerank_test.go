package tools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/topology"
	goext "github.com/plumbkit/plumb/internal/topology/extractors/golang"
)

// stubEmbedder returns deterministic vectors so the test controls the ranking.
type stubEmbedder struct {
	calls int
	fail  bool
}

func (e *stubEmbedder) Model() string { return "stub-model" }

func (e *stubEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.calls++
	if e.fail {
		return nil, fmt.Errorf("stub embed failure")
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		switch {
		case strings.Contains(t, "Cache"):
			out[i] = []float32{1, 0, 0}
		case strings.Contains(t, "Index"):
			out[i] = []float32{0, 1, 0}
		case strings.Contains(t, "Search"):
			out[i] = []float32{0, 0, 1}
		default:
			out[i] = []float32{1, 0, 0} // the query → aligned with Cache*
		}
	}
	return out, nil
}

func rerankFixture(t *testing.T) *topology.Store {
	t.Helper()
	ws := t.TempDir()
	src := "package demo\n\n" +
		"// SearchWidget searches widgets.\nfunc SearchWidget() {}\n\n" +
		"// IndexWidget indexes widgets.\nfunc IndexWidget() {}\n\n" +
		"// CacheWidget caches widgets.\nfunc CacheWidget() {}\n"
	if err := os.WriteFile(filepath.Join(ws, "demo.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := s.SymbolsInFile(context.Background(), filepath.Join(ws, "demo.go")); len(n) >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return s
}

func runSearch(t *testing.T, tool *tools.TopologySearch, args map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(args)
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return out
}

func TestTopologySearch_SemanticRerank(t *testing.T) {
	s := rerankFixture(t)
	emb := &stubEmbedder{}
	cfg := tools.SemanticRerankConfig{Enabled: true, Candidates: 50, Embedder: emb}
	tool := tools.NewTopologySearch(func() *topology.Store { return s }).
		WithSemantics(func() tools.SemanticRerankConfig { return cfg })

	out := runSearch(t, tool, map[string]any{"query": "widget"})
	if !strings.Contains(out, "mode=fts+semantic") {
		t.Errorf("expected semantic mode label; got:\n%s", out)
	}
	// The stub aligns the query with Cache*, so CacheWidget must be first.
	if !firstSymbolIs(out, "CacheWidget") {
		t.Errorf("rerank should put CacheWidget first; got:\n%s", out)
	}

	// Second identical search: candidate vectors are cached, so the embedder is
	// only called for the query (1 call), not the candidates.
	before := emb.calls
	_ = runSearch(t, tool, map[string]any{"query": "widget"})
	if emb.calls-before != 1 {
		t.Errorf("expected 1 embed call (query only, candidates cached); got %d", emb.calls-before)
	}
}

func TestTopologySearch_RerankDisabledAndFallback(t *testing.T) {
	s := rerankFixture(t)

	// rerank:false forces the plain FTS5 ranking even when configured.
	tool := tools.NewTopologySearch(func() *topology.Store { return s }).
		WithSemantics(func() tools.SemanticRerankConfig {
			return tools.SemanticRerankConfig{Enabled: true, Candidates: 50, Embedder: &stubEmbedder{}}
		})
	out := runSearch(t, tool, map[string]any{"query": "widget", "rerank": false})
	if !strings.Contains(out, "mode=ranked") {
		t.Errorf("rerank:false should keep FTS5 mode=ranked; got:\n%s", out)
	}

	// A failing embedder falls back to FTS5 (mode=ranked), never errors.
	failTool := tools.NewTopologySearch(func() *topology.Store { return s }).
		WithSemantics(func() tools.SemanticRerankConfig {
			return tools.SemanticRerankConfig{Enabled: true, Candidates: 50, Embedder: &stubEmbedder{fail: true}}
		})
	out = runSearch(t, failTool, map[string]any{"query": "widget"})
	if !strings.Contains(out, "mode=ranked") {
		t.Errorf("embed failure should fall back to FTS5 mode=ranked; got:\n%s", out)
	}
}

// firstSymbolIs reports whether name is the first result symbol in the output.
func firstSymbolIs(out, name string) bool {
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "function ") || strings.HasPrefix(l, "method ") {
			return strings.Contains(l, name)
		}
	}
	return false
}
