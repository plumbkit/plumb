package cli

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// The fan-out path reported a cold workspace as an EMPTY one. symbolsFrom took a
// single non-blocking handle check and returned ready=false, the loop skipped
// the target, and with nothing ready and nothing failed the merge returned
// (nil, nil) — a successful answer meaning "this workspace has no symbols".
//
// The primary-only path never did that: it blocks up to warmCap and then says
// "still warming". The two paths disagreed about the same question, and which
// one a workspace took depended only on whether it had a discovered set.

// installWarmingEntry installs a pool entry whose client is NOT yet set — the
// shape of a server that has been acquired and is still handshaking.
func installWarmingEntry(pool *workspacePool, root string) *clientProxy {
	cp := &clientProxy{}
	pool.entries[poolKey{root, "go"}] = &poolEntry{root: root, language: "go", proxy: cp}
	return cp
}

// TestWorkspaceSymbols_FanOutWaitsForAWarmingServer is the reported defect. The
// server becomes ready shortly after the query starts, which is the ordinary
// cold-start race, and the answer must be the symbols rather than silence.
func TestWorkspaceSymbols_FanOutWaitsForAWarmingServer(t *testing.T) {
	base := t.TempDir()
	core := filepath.Join(base, "core")
	pool := newTestPool()
	cp := installWarmingEntry(pool, core)

	rp := newRoutingProxy(pool)
	rp.setDiscovered(base, []discoveredRoot{{root: core, language: "go"}})

	// Ready after a few poll intervals — the server finishing its handshake
	// while the query waits.
	go func() {
		time.Sleep(3 * warmPollInterval)
		cp.set(&stubClient{symbols: []protocol.SymbolInformation{stubSym("CoreFn", "file://"+core+"/m.go")}})
	}()

	got, err := rp.WorkspaceSymbols(context.Background(), protocol.WorkspaceSymbolParams{Query: "Fn"})
	if err != nil {
		t.Fatalf("WorkspaceSymbols: %v — a warming server must be waited for, not skipped", err)
	}
	if names := stubSymNames(got); len(names) != 1 || names[0] != "CoreFn" {
		t.Errorf("fan-out symbols = %v, want [CoreFn] — the query returned before the "+
			"server was ready and reported the workspace as empty", names)
	}
}

// TestWorkspaceSymbols_FanOutReportsWarmingNotEmpty: when the wait elapses and
// NO target ever became ready, the answer must be the warming error, not an
// empty success. "No symbols here" and "no server could answer yet" are opposite
// facts and an agent cannot tell them apart from an empty slice — it concludes
// the symbol does not exist.
func TestWorkspaceSymbols_FanOutReportsWarmingNotEmpty(t *testing.T) {
	base := t.TempDir()
	core := filepath.Join(base, "core")
	pool := newTestPool()
	installWarmingEntry(pool, core) // never becomes ready

	rp := newRoutingProxy(pool)
	rp.warmCap = 100 * time.Millisecond // keep the test quick; the bound is the point, not its size
	rp.setDiscovered(base, []discoveredRoot{{root: core, language: "go"}})

	got, err := rp.WorkspaceSymbols(context.Background(), protocol.WorkspaceSymbolParams{Query: "Fn"})
	if err == nil {
		t.Fatalf("WorkspaceSymbols returned (%v, nil) — a workspace whose every server is "+
			"still warming must not answer 'no symbols'", stubSymNames(got))
	}
	if !strings.Contains(err.Error(), "warming") && !strings.Contains(err.Error(), "not yet ready") {
		t.Errorf("error = %q, want the warming/not-yet-ready message the primary path gives", err)
	}
}

// TestWorkspaceSymbols_FanOutStillAnswersFromTheReadyTargets: a warming target
// must not suppress a ready one. This is the half that stops the fix from
// overcorrecting into "any cold server fails the whole query".
func TestWorkspaceSymbols_FanOutStillAnswersFromTheReadyTargets(t *testing.T) {
	base := t.TempDir()
	core := filepath.Join(base, "core")
	app := filepath.Join(base, "app")
	pool := newTestPool()
	installEntry(pool, core, &stubClient{symbols: []protocol.SymbolInformation{stubSym("CoreFn", "file://"+core+"/m.go")}})
	installWarmingEntry(pool, app) // never ready

	rp := newRoutingProxy(pool)
	rp.warmCap = 100 * time.Millisecond
	rp.setDiscovered(base, []discoveredRoot{{root: core, language: "go"}, {root: app, language: "go"}})

	got, err := rp.WorkspaceSymbols(context.Background(), protocol.WorkspaceSymbolParams{Query: "Fn"})
	if err != nil {
		t.Fatalf("WorkspaceSymbols: %v — one cold target must not fail a query another answered", err)
	}
	if names := stubSymNames(got); len(names) != 1 || names[0] != "CoreFn" {
		t.Errorf("fan-out symbols = %v, want [CoreFn]", names)
	}
}

// TestWorkspaceSymbols_FanOutSharesOneDeadlineAcrossTargets: the wait is bounded
// ONCE for the whole fan-out, not per target. Four cold targets at warmCap each
// would be 4×warmCap — a query that reads as a hang on a cold daemon. Asserted
// as elapsed time, because that is the property; a per-target bound would pass
// every other test in this file.
func TestWorkspaceSymbols_FanOutSharesOneDeadlineAcrossTargets(t *testing.T) {
	base := t.TempDir()
	pool := newTestPool()
	names := []string{"a", "b", "c", "d"}
	discovered := make([]discoveredRoot, 0, len(names))
	for _, name := range names {
		root := filepath.Join(base, name)
		installWarmingEntry(pool, root)
		discovered = append(discovered, discoveredRoot{root: root, language: "go"})
	}

	rp := newRoutingProxy(pool)
	rp.warmCap = 150 * time.Millisecond
	rp.setDiscovered(base, discovered)

	start := time.Now()
	if _, err := rp.WorkspaceSymbols(context.Background(), protocol.WorkspaceSymbolParams{Query: "Fn"}); err == nil {
		t.Fatal("want the warming error with every target cold")
	}
	// Generous headroom over one warmCap, but far below the four the per-target
	// shape would cost.
	if elapsed := time.Since(start); elapsed > 3*rp.warmCap {
		t.Errorf("fan-out took %s for 4 cold targets with warmCap=%s — the deadline must be "+
			"shared across the fan-out, not spent once per target", elapsed, rp.warmCap)
	}
}

// TestSymbolTargets_EveryDiscoveredRootBecomesATarget pins the invariant that
// lets the warming return above be unconditional: the fan-out is only entered
// with a non-empty discovered set, and every entry in it becomes a target, so
// "no targets" cannot occur there.
//
// This replaces a test that claimed to cover the no-targets case and could not:
// it built an EMPTY discovered set, which takes the primary-only path and never
// reaches the fan-out at all, and its assertion (`err == nil && len(got) != 0`)
// passed whenever an error was returned — the one outcome it existed to reject.
func TestSymbolTargets_EveryDiscoveredRootBecomesATarget(t *testing.T) {
	base := t.TempDir()
	core := filepath.Join(base, "core")
	app := filepath.Join(base, "app")
	rp := newRoutingProxy(newTestPool())

	discovered := []discoveredRoot{{root: core, language: "go"}, {root: app, language: "typescript"}}
	targets := rp.symbolTargets(base, discovered)

	if len(targets) < len(discovered) {
		t.Fatalf("symbolTargets = %v for %d discovered roots — a discovered root that "+
			"produced no target would make the fan-out's unconditional warming return "+
			"reachable with nothing to wait for", targets, len(discovered))
	}
	for _, d := range discovered {
		if !slices.Contains(targets, lsTarget{root: d.root, language: d.language}) {
			t.Errorf("symbolTargets %v is missing discovered root %s/%s", targets, d.root, d.language)
		}
	}
}
