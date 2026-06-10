package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/memory"
	"github.com/plumbkit/plumb/internal/topology"
	goext "github.com/plumbkit/plumb/internal/topology/extractors/golang"
	"github.com/plumbkit/plumb/internal/topology/extractors/treesitter"
)

const wsSearchGoFixture = `package demo

// AcquireDaemonLock serialises daemon startup.
func AcquireDaemonLock() {}
`

const wsSearchDocFixture = `# Daemon locking

How the daemon lock works.
`

// openWorkspaceSearchFixture builds a workspace with one Go symbol, one
// Markdown section, and one memory, all indexed, and returns a wired tool.
func openWorkspaceSearchFixture(t *testing.T) (*WorkspaceSearch, string) {
	t.Helper()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "demo.go"), []byte(wsSearchGoFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "guide.md"), []byte(wsSearchDocFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New(), treesitter.NewMarkdown()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ix, err := memory.OpenIndex(ws)
	if err != nil {
		t.Fatalf("memory.OpenIndex: %v", err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	if err := memory.WriteIndexed(ix, ws, "daemon-locking", "The daemon lock is a flock.", "Why daemon locking works this way"); err != nil {
		t.Fatalf("WriteIndexed: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// One term that matches all topology fixtures (FTS is AND-of-terms).
		hits, _ := store.Search(context.Background(), "daemon", topology.SearchOpts{Limit: 10})
		if len(hits) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	tool := NewWorkspaceSearch(func() string { return ws }, func() *topology.Store { return store }).
		WithMemoryIndex(func() *memory.Index { return ix })
	return tool, ws
}

func runWorkspaceSearch(t *testing.T, tool *WorkspaceSearch, args map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(args)
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("Execute(%v): %v", args, err)
	}
	return out
}

func TestWorkspaceSearch_AllCorporaLabelled(t *testing.T) {
	tool, _ := openWorkspaceSearchFixture(t)
	out := runWorkspaceSearch(t, tool, map[string]any{"query": "daemon"})

	for _, want := range []string{"[code]", "[docs]", "[memory]"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %s hit:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "exact_match=false") {
		t.Errorf("output must state exact_match=false:\n%s", out)
	}
	if !strings.Contains(out, "source=topology-fts") || !strings.Contains(out, "source=memory-fts") {
		t.Errorf("results must state their source:\n%s", out)
	}
	if !strings.Contains(out, "why=") || !strings.Contains(out, "score=") || !strings.Contains(out, "field=") {
		t.Errorf("results must carry why/score/field labels:\n%s", out)
	}
}

func TestWorkspaceSearch_MemoryOnlyDegradation(t *testing.T) {
	// No topology store: code/docs report missing, memory still serves.
	ws := t.TempDir()
	ix, err := memory.OpenIndex(ws)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	if err := memory.WriteIndexed(ix, ws, "daemon-locking", "The daemon lock is a flock.", "Why daemon locking works"); err != nil {
		t.Fatal(err)
	}

	tool := NewWorkspaceSearch(func() string { return ws }, func() *topology.Store { return nil }).
		WithMemoryIndex(func() *memory.Index { return ix })
	out := runWorkspaceSearch(t, tool, map[string]any{"query": "daemon locking"})

	if !strings.Contains(out, "code/docs=missing") {
		t.Errorf("topology corpora must report missing:\n%s", out)
	}
	if !strings.Contains(out, "[memory] daemon-locking") {
		t.Errorf("memory corpus should still serve:\n%s", out)
	}
}

func TestWorkspaceSearch_NoMemoryIndexReportsMissing(t *testing.T) {
	tool, _ := openWorkspaceSearchFixture(t)
	tool.memFn = nil
	out := runWorkspaceSearch(t, tool, map[string]any{"query": "daemon"})

	if !strings.Contains(out, "memory=missing") {
		t.Errorf("absent memory index must report missing:\n%s", out)
	}
	if strings.Contains(out, "[memory]") {
		t.Errorf("no memory hits expected without an index:\n%s", out)
	}
}

func TestWorkspaceSearch_CorporaFilter(t *testing.T) {
	tool, _ := openWorkspaceSearchFixture(t)
	out := runWorkspaceSearch(t, tool, map[string]any{"query": "daemon", "corpora": []string{"memory"}})

	if strings.Contains(out, "[code]") || strings.Contains(out, "[docs]") {
		t.Errorf("corpora filter must exclude topology hits:\n%s", out)
	}
	if !strings.Contains(out, "code/docs=skipped") {
		t.Errorf("skipped corpora must be labelled:\n%s", out)
	}
	if !strings.Contains(out, "[memory]") {
		t.Errorf("memory corpus requested but absent:\n%s", out)
	}
}

func TestWorkspaceSearch_EmptyPointsAtExactScan(t *testing.T) {
	tool, _ := openWorkspaceSearchFixture(t)
	out := runWorkspaceSearch(t, tool, map[string]any{"query": "zzqx-nothing-matches"})

	if !strings.Contains(out, "No indexed matches") || !strings.Contains(out, "search_in_files") {
		t.Errorf("empty result must say so and point at the exact scanner:\n%s", out)
	}
}

func TestWorkspaceSearch_RejectsBadArgs(t *testing.T) {
	tool := NewWorkspaceSearch(func() string { return "" }, func() *topology.Store { return nil })
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"query": ""}`)); err == nil {
		t.Error("empty query must be rejected")
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"query": "x", "corpora": ["nope"]}`)); err == nil {
		t.Error("unknown corpus must be rejected")
	}
}

func TestInterleaveHits(t *testing.T) {
	a := []wsHit{{label: "a1"}, {label: "a2"}, {label: "a3"}}
	b := []wsHit{{label: "b1"}}
	c := []wsHit{{label: "c1"}, {label: "c2"}}

	got := interleaveHits(4, a, b, c)
	want := []string{"a1", "b1", "c1", "a2"}
	if len(got) != len(want) {
		t.Fatalf("got %d hits, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].label != w {
			t.Errorf("hit %d = %q, want %q (round-robin by per-corpus rank)", i, got[i].label, w)
		}
	}
	if got := interleaveHits(10, a, b, c); len(got) != 6 {
		t.Errorf("exhausting all lists should return every hit, got %d", len(got))
	}
}
