package cli

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

func stubSym(name, uri string) protocol.SymbolInformation {
	return protocol.SymbolInformation{
		Name:     name,
		Kind:     protocol.SKFunction,
		Location: protocol.Location{URI: uri},
	}
}

func stubSymNames(syms []protocol.SymbolInformation) []string {
	out := make([]string, 0, len(syms))
	for _, s := range syms {
		out = append(out, s.Name)
	}
	slices.Sort(out)
	return out
}

// TestWorkspaceSymbols_FanOutMerges: a monorepo root with discovered child
// language roots queries every child server and merges the results, so a no-file
// symbol search spans every detected language rather than the primary alone.
func TestWorkspaceSymbols_FanOutMerges(t *testing.T) {
	base := t.TempDir()
	core := filepath.Join(base, "core")
	app := filepath.Join(base, "app")
	pool := newTestPool()
	installEntry(pool, core, &stubClient{symbols: []protocol.SymbolInformation{stubSym("CoreFn", "file://"+core+"/m.go")}})
	installEntry(pool, app, &stubClient{symbols: []protocol.SymbolInformation{stubSym("AppFn", "file://"+app+"/m.go")}})

	rp := newRoutingProxy(pool)
	rp.setPrimary(core, "go", pool.entries[poolKey{core, "go"}].proxy)
	rp.setDiscovered(base, []discoveredRoot{{root: core, language: "go"}, {root: app, language: "go"}})

	got, err := rp.WorkspaceSymbols(context.Background(), protocol.WorkspaceSymbolParams{Query: "Fn"})
	if err != nil {
		t.Fatalf("WorkspaceSymbols: %v", err)
	}
	if names := stubSymNames(got); !slices.Equal(names, []string{"AppFn", "CoreFn"}) {
		t.Errorf("fan-out symbols = %v, want [AppFn CoreFn]", names)
	}
}

// TestWorkspaceSymbols_FanOutDedups: an identical symbol surfaced by two servers
// (a file on a root boundary) appears once in the merged result.
func TestWorkspaceSymbols_FanOutDedups(t *testing.T) {
	base := t.TempDir()
	core := filepath.Join(base, "core")
	app := filepath.Join(base, "app")
	dup := stubSym("Shared", "file://"+base+"/shared.go")
	pool := newTestPool()
	installEntry(pool, core, &stubClient{symbols: []protocol.SymbolInformation{dup}})
	installEntry(pool, app, &stubClient{symbols: []protocol.SymbolInformation{dup}})

	rp := newRoutingProxy(pool)
	rp.setPrimary(core, "go", pool.entries[poolKey{core, "go"}].proxy)
	rp.setDiscovered(base, []discoveredRoot{{root: core, language: "go"}, {root: app, language: "go"}})

	got, err := rp.WorkspaceSymbols(context.Background(), protocol.WorkspaceSymbolParams{Query: "Shared"})
	if err != nil {
		t.Fatalf("WorkspaceSymbols: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("dedup: got %d symbols, want 1", len(got))
	}
}

// TestWorkspaceSymbols_NoDiscoveredUsesPrimary: a single-language root (no
// discovered set) keeps the primary-only path.
func TestWorkspaceSymbols_NoDiscoveredUsesPrimary(t *testing.T) {
	rootA, _ := setupTwoProjects(t)
	pool := newTestPool()
	installEntry(pool, rootA, &stubClient{symbols: []protocol.SymbolInformation{stubSym("Only", "file://"+rootA+"/m.go")}})

	rp := newRoutingProxy(pool)
	rp.setPrimary(rootA, "go", pool.entries[poolKey{rootA, "go"}].proxy)
	// setDiscovered with nil (the single-language case) must not trigger fan-out.
	rp.setDiscovered(rootA, nil)

	got, err := rp.WorkspaceSymbols(context.Background(), protocol.WorkspaceSymbolParams{Query: "Only"})
	if err != nil {
		t.Fatalf("WorkspaceSymbols: %v", err)
	}
	if len(got) != 1 || got[0].Name != "Only" {
		t.Errorf("primary path symbols = %v, want [Only]", stubSymNames(got))
	}
}
