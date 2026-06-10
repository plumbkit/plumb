package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/memory"
	"github.com/plumbkit/plumb/internal/topology"
)

// TestStaleSymbolsNote uses the structural fixture (DocumentedExport et al.
// indexed) to prove the stale check flags only symbols missing from the code
// map, and stays silent when everything resolves.
func TestStaleSymbolsNote(t *testing.T) {
	store, _ := openStructuralFixture(t)
	ctx := context.Background()

	rec := memory.Record{SourceSymbols: []string{"DocumentedExport", "VanishedSymbol"}}
	note := staleSymbolsNote(ctx, store, rec)
	if !strings.Contains(note, "VanishedSymbol") {
		t.Errorf("missing symbol should be flagged:\n%s", note)
	}
	if strings.Contains(note, "DocumentedExport") {
		t.Errorf("live symbol must not be flagged:\n%s", note)
	}

	if note := staleSymbolsNote(ctx, store, memory.Record{SourceSymbols: []string{"DocumentedExport"}}); note != "" {
		t.Errorf("all-live symbols should produce no note, got %q", note)
	}
	// A dotted reference (the form read_symbol/find_symbol args accept) must
	// resolve by its base segment, not be falsely flagged stale.
	if note := staleSymbolsNote(ctx, store, memory.Record{SourceSymbols: []string{"demo.DocumentedExport"}}); note != "" {
		t.Errorf("dotted reference to a live symbol must not be flagged, got %q", note)
	}
	if note := staleSymbolsNote(ctx, nil, rec); note != "" {
		t.Errorf("nil store should produce no note, got %q", note)
	}
	if note := staleSymbolsNote(ctx, store, memory.Record{}); note != "" {
		t.Errorf("no referenced symbols should produce no note, got %q", note)
	}
}

// TestTopologyExplore_RelatedMemories proves the CodeRef join end-to-end: a
// memory whose provenance references an explored symbol appears in the
// topology_explore response, names-and-why only.
func TestTopologyExplore_RelatedMemories(t *testing.T) {
	store, ws := openStructuralFixture(t)
	prov := memory.Provenance{SourceSymbols: []string{"DocumentedExport"}}
	if err := memory.WriteGenerated(nil, ws, "export-design", "why DocumentedExport exists", "body", prov); err != nil {
		t.Fatal(err)
	}

	tool := NewTopologyExplore(func() *topology.Store { return store }).
		WithMemories(func() string { return ws })
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"DocumentedExport"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "Related memories") || !strings.Contains(out, "'export-design'") {
		t.Errorf("explore should list the related memory:\n%s", out)
	}
	if !strings.Contains(out, "references symbol DocumentedExport") {
		t.Errorf("the match reason should be stated:\n%s", out)
	}
	if strings.Contains(out, "body") {
		t.Errorf("memory bodies must never be inlined:\n%s", out)
	}
}

// TestTopologyAffected_KnownContext: a memory glob-attached to a changed file
// surfaces on the affected report.
func TestTopologyAffected_KnownContext(t *testing.T) {
	store, ws := openStructuralFixture(t)
	if err := memory.Write(ws, "demo-risk", "demo.go is risky", "Touching demo.go needs care"); err != nil {
		t.Fatal(err)
	}
	// Attach via paths glob frontmatter.
	content := "---\nname: demo-risk\ndescription: Touching demo.go needs care\npaths: demo.go\n---\n\nbody"
	if err := memory.Write(ws, "demo-risk", content, ""); err != nil {
		t.Fatal(err)
	}

	tool := NewTopologyAffected(func() *topology.Store { return store }).
		WithMemories(func() string { return ws })
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"files":["demo.go"]}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "'demo-risk'") {
		t.Errorf("affected report should surface the attached memory:\n%s", out)
	}
}

func TestRecentFirstMemories(t *testing.T) {
	mems := []memory.Memory{
		{Name: "aaa-cold", Paths: []string{"docs/**"}},
		{Name: "bbb-hot", Paths: []string{"internal/cli/**"}},
		{Name: "ccc-cold", Paths: []string{"web/**"}},
	}
	got := recentFirstMemories(mems, []string{"internal/cli/lock.go"})
	if got[0].Name != "bbb-hot" {
		t.Errorf("recently-relevant memory should lead, got %v", got[0].Name)
	}
	if len(got) != 3 {
		t.Fatalf("partition must keep every memory, got %d", len(got))
	}
	// Empty recent list keeps the original order.
	got = recentFirstMemories(mems, nil)
	if got[0].Name != "aaa-cold" {
		t.Errorf("no recent files should keep name order, got %v", got[0].Name)
	}
}
