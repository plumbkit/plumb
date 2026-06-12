package tools_test

import (
	"context"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
)

func loc(uri string, line uint32) protocol.Location {
	return protocol.Location{URI: uri, Range: protocol.Range{Start: protocol.Position{Line: line}}}
}

// TestLSPCrossFileCallers_FiltersSelfAndDedupes verifies the resolver returns
// only references outside the symbol's own file, deduped and 1-based, and that
// it queries the identifier position (SelectionRange) — not the declaration
// keyword.
func TestLSPCrossFileCallers_FiltersSelfAndDedupes(t *testing.T) {
	mock := &mockLSP{
		docSymbols: symbolWithKeywordRange("RecoveredHijacks"),
		locations: []protocol.Location{
			loc("file:///p/paths.go", 10),       // self file → excluded
			loc("file:///p/cli/hijack.go", 26),  // cross-file
			loc("file:///p/cli/hijack.go", 41),  // cross-file
			loc("file:///p/cli/hijack.go", 41),  // duplicate → collapsed
		},
	}
	fn := tools.NewLSPCrossFileCallers(mock, nil, time.Minute, 0)
	if fn == nil {
		t.Fatal("expected a non-nil resolver for a non-nil client")
	}

	sites := fn(context.Background(), "/p/paths.go", "RecoveredHijacks")

	if len(sites) != 2 {
		t.Fatalf("expected 2 distinct cross-file sites, got %d: %+v", len(sites), sites)
	}
	// 1-based lines, sorted; self-file reference excluded.
	if sites[0] != (tools.CallerSite{Path: "/p/cli/hijack.go", Line: 27}) ||
		sites[1] != (tools.CallerSite{Path: "/p/cli/hijack.go", Line: 42}) {
		t.Errorf("unexpected sites: %+v", sites)
	}
	if want := (protocol.Position{Line: 5, Character: 5}); mock.lastRefPos != want {
		t.Errorf("References queried at %+v, want the identifier (SelectionRange) %+v", mock.lastRefPos, want)
	}
}

// TestLSPCrossFileCallers_NilClient confirms a nil client yields a nil resolver,
// so topology_impact stays topology-only when no language server is wired.
func TestLSPCrossFileCallers_NilClient(t *testing.T) {
	if fn := tools.NewLSPCrossFileCallers(nil, nil, 0, 0); fn != nil {
		t.Error("expected nil resolver for a nil client")
	}
}

// TestLSPCrossFileCallers_UnknownSymbol returns nil when the symbol is absent
// from the file's document symbols (no position to query from).
func TestLSPCrossFileCallers_UnknownSymbol(t *testing.T) {
	mock := &mockLSP{docSymbols: symbolWithKeywordRange("Other")}
	fn := tools.NewLSPCrossFileCallers(mock, nil, time.Minute, 0)
	if sites := fn(context.Background(), "/p/paths.go", "Missing"); sites != nil {
		t.Errorf("expected nil for an unknown symbol, got %+v", sites)
	}
}
