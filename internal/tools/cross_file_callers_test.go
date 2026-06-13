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

// TestLSPCrossFileCallers_RelativePathAndFiltering exercises the whole resolver
// against a workspace-RELATIVE centre path (as topology supplies it). It guards
// three things at once: the relative path is absolutised before the LSP query
// (regression — a relative file:// URI made gopls return nothing live); the
// symbol's own file is excluded; and the surviving sites are deduped, 1-based,
// sorted, and re-relativised to the workspace root.
func TestLSPCrossFileCallers_RelativePathAndFiltering(t *testing.T) {
	const root = "/ws"
	mock := &mockLSP{
		docSymbols: symbolWithKeywordRange("RecoveredHijacks"),
		locations: []protocol.Location{
			loc("file:///ws/internal/paths/paths.go", 10), // self file → excluded
			loc("file:///ws/internal/cli/hijack.go", 26),  // cross-file
			loc("file:///ws/internal/cli/hijack.go", 41),  // cross-file
			loc("file:///ws/internal/cli/hijack.go", 41),  // duplicate → collapsed
		},
	}
	fn := tools.NewLSPCrossFileCallers(mock, nil, time.Minute, 0, func() string { return root })
	if fn == nil {
		t.Fatal("expected a non-nil resolver for a non-nil client")
	}

	// Relative centre path, exactly as topology stores it.
	sites := fn(context.Background(), "internal/paths/paths.go", "RecoveredHijacks")

	// The LSP must be queried with an ABSOLUTE file URI, not the relative path.
	if want := "file:///ws/internal/paths/paths.go"; mock.lastRefURI != want {
		t.Errorf("References queried URI %q, want absolute %q (relative path was not absolutised)", mock.lastRefURI, want)
	}
	if want := (protocol.Position{Line: 5, Character: 5}); mock.lastRefPos != want {
		t.Errorf("References queried at %+v, want the identifier (SelectionRange) %+v", mock.lastRefPos, want)
	}
	if len(sites) != 2 {
		t.Fatalf("expected 2 distinct cross-file sites, got %d: %+v", len(sites), sites)
	}
	// Self-file excluded; cross-file sites deduped, 1-based, workspace-relative.
	if sites[0] != (tools.CallerSite{Path: "internal/cli/hijack.go", Line: 27}) ||
		sites[1] != (tools.CallerSite{Path: "internal/cli/hijack.go", Line: 42}) {
		t.Errorf("unexpected sites: %+v", sites)
	}
}

// TestLSPCrossFileCallers_NilClient confirms a nil client yields a nil resolver,
// so topology_impact stays topology-only when no language server is wired.
func TestLSPCrossFileCallers_NilClient(t *testing.T) {
	if fn := tools.NewLSPCrossFileCallers(nil, nil, 0, 0, func() string { return "/ws" }); fn != nil {
		t.Error("expected nil resolver for a nil client")
	}
}

// TestLSPCrossFileCallers_UnknownSymbol returns nil when the symbol is absent
// from the file's document symbols (no position to query from).
func TestLSPCrossFileCallers_UnknownSymbol(t *testing.T) {
	mock := &mockLSP{docSymbols: symbolWithKeywordRange("Other")}
	fn := tools.NewLSPCrossFileCallers(mock, nil, time.Minute, 0, func() string { return "/ws" })
	if sites := fn(context.Background(), "internal/paths/paths.go", "Missing"); sites != nil {
		t.Errorf("expected nil for an unknown symbol, got %+v", sites)
	}
}
