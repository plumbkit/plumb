package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp"
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

// ctxHonouringClient is a stub whose WorkspaceSymbols respects the context and
// costs a little time, like a real stdio RPC. The plain stubClient ignores ctx
// and returns instantly, which is precisely why the first version of these tests
// could not see the starvation defect below: with a zero-cost stub the whole
// loop finished inside the microseconds between the deadline being set and the
// first wait computing its remaining time.
type ctxHonouringClient struct {
	lsp.Client
	symbols []protocol.SymbolInformation
	cost    time.Duration
	fail    error
}

func (c *ctxHonouringClient) WorkspaceSymbols(ctx context.Context, _ protocol.WorkspaceSymbolParams) ([]protocol.SymbolInformation, error) {
	if c.cost > 0 {
		select {
		case <-time.After(c.cost):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.fail != nil {
		return nil, c.fail
	}
	return c.symbols, nil
}

var errFanOutTargetFailed = errors.New("stub: this server is broken")

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

// TestWorkspaceSymbols_FanOutColdTargetFirstDoesNotStarveTheRest is the
// regression pin for a defect review found in the first version of this fix. The
// shared budget bounded the WHOLE fan-out rather than the waiting, so a cold
// target ordered first spent it, and every warm target after it issued its query
// on an already-expired context and failed with "context deadline exceeded" —
// losing symbols the fan-out used to return instantly, and surfacing that error
// instead of the results.
//
// The ordering is the whole test: the sibling above puts the READY target first,
// so it passes either way. symbolTargets lists discovered child roots before
// attached entries, so cold-first is the ordinary monorepo shape, not a contrived
// one. The clients honour ctx and cost time, or the race closes too fast to see.
func TestWorkspaceSymbols_FanOutColdTargetFirstDoesNotStarveTheRest(t *testing.T) {
	base := t.TempDir()
	cold := filepath.Join(base, "cold")
	pool := newTestPool()
	installWarmingEntry(pool, cold) // never ready, and FIRST

	discovered := make([]discoveredRoot, 0, 4)
	discovered = append(discovered, discoveredRoot{root: cold, language: "go"})
	want := 0
	for _, name := range []string{"b", "c", "d"} {
		root := filepath.Join(base, name)
		installEntry(pool, root, &ctxHonouringClient{
			symbols: []protocol.SymbolInformation{stubSym("Fn"+name, "file://"+root+"/m.go")},
			cost:    2 * time.Millisecond,
		})
		discovered = append(discovered, discoveredRoot{root: root, language: "go"})
		want++
	}

	rp := newRoutingProxy(pool)
	rp.warmCap = 100 * time.Millisecond
	rp.setDiscovered(base, discovered)

	got, err := rp.WorkspaceSymbols(context.Background(), protocol.WorkspaceSymbolParams{Query: "Fn"})
	if err != nil {
		t.Fatalf("WorkspaceSymbols: %v — three warm servers were ready and holding symbols; "+
			"a cold target listed before them must cost them nothing", err)
	}
	if names := stubSymNames(got); len(names) != want {
		t.Errorf("fan-out symbols = %v, want %d — the warm targets' results were lost to the "+
			"budget the cold one spent", names, want)
	}
}

// TestWorkspaceSymbols_FanOutWarmingErrorCarriesTheElapsedHint: the warming
// message distinguishes "not yet ready" from "still warming (N elapsed)", and
// the second form is what tells an agent whether retrying is worth it. Mutation
// showed longestWarmup could be gutted to `return 0` and pass the entire
// package — nothing asserted the hint it exists to produce.
func TestWorkspaceSymbols_FanOutWarmingErrorCarriesTheElapsedHint(t *testing.T) {
	base := t.TempDir()
	cold := filepath.Join(base, "cold")
	pool := newTestPool()
	installWarmingEntry(pool, cold)
	// A server that began warming a while ago: warmupFor reports the elapsed time
	// from startedAt, which is what the hint is built from.
	pool.entries[poolKey{cold, "go"}].startedAt = time.Now().Add(-90 * time.Second)

	rp := newRoutingProxy(pool)
	rp.warmCap = 50 * time.Millisecond
	rp.setDiscovered(base, []discoveredRoot{{root: cold, language: "go"}})

	_, err := rp.WorkspaceSymbols(context.Background(), protocol.WorkspaceSymbolParams{Query: "Fn"})
	if err == nil {
		t.Fatal("want the warming error")
	}
	if !strings.Contains(err.Error(), "elapsed") {
		t.Errorf("error = %q, want the \"(N elapsed)\" hint — a server 90s into its start is "+
			"a different situation from one that just began, and the caller cannot tell "+
			"without it", err)
	}
}

// TestWorkspaceSymbols_FanOutSurfacesAFailureOverWarming: when a target genuinely
// ERRORS and none is ready, the error is what the caller needs, not the warming
// message. Mutation showed the precedence was asserted only by a comment — no
// test drove a failing target at all.
func TestWorkspaceSymbols_FanOutSurfacesAFailureOverWarming(t *testing.T) {
	base := t.TempDir()
	bad := filepath.Join(base, "bad")
	cold := filepath.Join(base, "cold")
	pool := newTestPool()
	installEntry(pool, bad, &ctxHonouringClient{fail: errFanOutTargetFailed})
	installWarmingEntry(pool, cold)

	rp := newRoutingProxy(pool)
	rp.warmCap = 50 * time.Millisecond
	rp.setDiscovered(base, []discoveredRoot{{root: bad, language: "go"}, {root: cold, language: "go"}})

	_, err := rp.WorkspaceSymbols(context.Background(), protocol.WorkspaceSymbolParams{Query: "Fn"})
	if err == nil {
		t.Fatal("want an error when a target failed and none answered")
	}
	if !strings.Contains(err.Error(), errFanOutTargetFailed.Error()) {
		t.Errorf("error = %q, want the target's own failure — a real failure must not be "+
			"reported as \"still warming\", which invites a pointless retry", err)
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

// TestWorkspaceSymbols_AgentRootWithNothingAttachedAnswersEmpty covers the
// caller that makes "no targets" reachable. agentWorkspaceSymbols fans out over
// a declared agent's own root with discovered = nil, so an agent whose root has
// no attached server queries nothing — and must be told its workspace is empty,
// not that it is "still warming", because there is no server to wait for.
//
// This case arrived from main while this branch was open: the guard it needs had
// been removed here as unreachable, on reasoning that was true of the only
// caller that existed at the time.
func TestWorkspaceSymbols_AgentRootWithNothingAttachedAnswersEmpty(t *testing.T) {
	base := t.TempDir()
	agentRoot := filepath.Join(base, "agent")
	if err := os.MkdirAll(agentRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	rp := newRoutingProxy(newTestPool()) // nothing attached anywhere

	got, err := rp.fanOutWorkspaceSymbols(context.Background(),
		protocol.WorkspaceSymbolParams{Query: "Fn"}, agentRoot, nil)
	if err != nil {
		t.Fatalf("fanOutWorkspaceSymbols: %v — with no target to query there is nothing "+
			"warming, and an empty answer is the honest one", err)
	}
	if len(got) != 0 {
		t.Errorf("symbols = %v, want empty", stubSymNames(got))
	}
}

// TestSymbolTargets_EveryDiscoveredRootBecomesATarget: every discovered root
// becomes a target, so a workspace that discovered languages always has
// something to query.
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
