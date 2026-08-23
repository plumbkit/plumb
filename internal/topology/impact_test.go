package topology

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// wideCallerGraph builds a star: one centre with n inward "calls" edges. It is
// the shape that makes a node budget observable — every neighbour sits one hop
// away, so the traversal is bounded by the budget and nothing else.
func wideCallerGraph(t *testing.T, n int) (*sql.DB, Node) {
	t.Helper()
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "impact.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	fileID := insertTestFile(t, db, "wide.go")
	centreID := insertTestNode(t, db, fileID, "wide.go", Node{Kind: KindFunction, Name: "Centre", Language: "go"})
	for i := range n {
		callerID := insertTestNode(t, db, fileID, "wide.go",
			Node{Kind: KindFunction, Name: "Caller" + strconv.Itoa(i), Language: "go"})
		insertTestEdge(t, db, callerID, centreID, string(EdgeCalls))
	}
	return db, Node{ID: centreID, Kind: KindFunction, Name: "Centre", Path: "wide.go", Language: "go"}
}

// TestImpactFrom_TraversalRunsWithTheBudgetTheCallerAsked is the regression
// guard for the defect that shipped: topology_affected sized a 2000-node budget
// for this traversal and clampImpactOpts cut it to 200 without saying so.
//
// The assertion is on what the BFS RETURNED — the effective budget — not on the
// value handed in. Every test that only checked the input passed over the bug
// for as long as it existed.
//
// It is written as a relationship (effective == requested) rather than against
// the literal 2000, so a later change to either constant cannot re-open the
// divergence while this stays green.
func TestImpactFrom_TraversalRunsWithTheBudgetTheCallerAsked(t *testing.T) {
	// Deliberately above the ceiling the MCP tool schemas advertise: an
	// in-process caller sizes its own budget and is not an untrusted argument.
	const requested = 600
	if requested <= toolCapNodes {
		t.Fatalf("test is vacuous: %d must exceed the tool-argument ceiling %d", requested, toolCapNodes)
	}
	db, centre := wideCallerGraph(t, requested)

	res, err := ImpactFrom(context.Background(), db, centre, ImpactOpts{
		Depth:     2,
		MaxNodes:  requested,
		MaxBytes:  hardCapBytes,
		EdgeKinds: []string{string(EdgeCalls)},
	})
	if err != nil {
		t.Fatalf("ImpactFrom: %v", err)
	}
	if got := len(res.DependedOnBy.Nodes); got != requested {
		t.Errorf("inward BFS returned %d nodes for a %d-node budget; the traversal is "+
			"running with a smaller budget than the caller asked for", got, requested)
	}
	if res.DependedOnBy.Truncated {
		t.Error("inward BFS reported truncation although the graph fits the requested budget")
	}
}

// TestImpactFrom_BudgetIsStillACeiling is the other direction. The test above
// alone would stay green if the cap were deleted outright, which would let one
// pathological root walk the whole index on the post-edit hot path.
func TestImpactFrom_BudgetIsStillACeiling(t *testing.T) {
	const graph = 60
	const requested = 7
	db, centre := wideCallerGraph(t, graph)

	res, err := ImpactFrom(context.Background(), db, centre, ImpactOpts{
		Depth:     2,
		MaxNodes:  requested,
		MaxBytes:  hardCapBytes,
		EdgeKinds: []string{string(EdgeCalls)},
	})
	if err != nil {
		t.Fatalf("ImpactFrom: %v", err)
	}
	if got := len(res.DependedOnBy.Nodes); got != requested {
		t.Errorf("inward BFS returned %d nodes for a %d-node budget over a %d-node graph; "+
			"a budget below the graph size must still bound the walk", got, requested, graph)
	}
	if !res.DependedOnBy.Truncated {
		t.Error("a walk cut short by the budget must report Truncated, or the caller reads " +
			"a partial answer as a complete one")
	}
}

// TestClampImpactOpts_BoundsAndDefaults pins the three things the clamp owes a
// caller, as relationships rather than literals.
func TestClampImpactOpts_BoundsAndDefaults(t *testing.T) {
	// 1. A deliberate in-process budget survives untouched.
	const budget = 2000
	if got := clampImpactOpts(ImpactOpts{MaxNodes: budget}).MaxNodes; got != budget {
		t.Errorf("effective MaxNodes = %d, want the requested %d", got, budget)
	}
	// 2. An unspecified budget still gets a default.
	if got := clampImpactOpts(ImpactOpts{}).MaxNodes; got != defaultImpactMaxNodes {
		t.Errorf("unspecified MaxNodes = %d, want the default %d", got, defaultImpactMaxNodes)
	}
	// 3. An unbounded request is still bounded. Asserted against the constant AND
	// against the request, so deleting the ceiling fails here rather than merely
	// changing a number.
	got := clampImpactOpts(ImpactOpts{MaxNodes: math.MaxInt32}).MaxNodes
	if got != hardCapNodes {
		t.Errorf("an unbounded request resolved to %d, want the traversal ceiling %d", got, hardCapNodes)
	}
	if got >= math.MaxInt32 {
		t.Errorf("an unbounded request was not bounded at all: %d", got)
	}
}

// TestClampToolNodes_KeepsTheAdvertisedCeiling covers the value that DID have to
// stay at 200: a max_nodes that arrived as an MCP tool argument. Separating the
// two ceilings is the fix; collapsing them again is the bug.
func TestClampToolNodes_KeepsTheAdvertisedCeiling(t *testing.T) {
	if got := ClampToolNodes(math.MaxInt32); got != toolCapNodes {
		t.Errorf("an over-cap tool argument resolved to %d, want %d", got, toolCapNodes)
	}
	if got := ClampToolNodes(10); got != 10 {
		t.Errorf("an under-cap tool argument was altered: got %d, want 10", got)
	}
	if got := ClampToolNodes(0); got != 0 {
		t.Errorf("an unspecified tool argument must stay 0 so the traversal default "+
			"applies; got %d", got)
	}
	if toolCapNodes > hardCapNodes {
		t.Errorf("the tool-argument ceiling (%d) exceeds the traversal ceiling (%d), so "+
			"clamping a tool argument would no longer bound anything", toolCapNodes, hardCapNodes)
	}
}

// TestTraversalCeilingsBindInThatOrder pins the relationship that makes both
// traversal ceilings live.
//
// Before PLAN-407 the pair was 5000 nodes × 100000 bytes. At the measured cost
// of a real node the byte ceiling bound at roughly 660, so the node ceiling
// could never fire — an inert cap that read as protective, which is the class of
// defect this card exists to remove. Swapping which of the two is decoration
// would be no better, so the assertion is on the ORDER they bind in, not on
// either constant.
//
// It goes through estimateBytes rather than arithmetic on the constants, so a
// change to what a node costs is caught here too.
func TestTraversalCeilingsBindInThatOrder(t *testing.T) {
	// A node engineered to cost exactly maxNodeBytes, the size the pair is sized
	// against.
	n := Node{Name: strings.Repeat("x", maxNodeBytes-50)}
	if got := estimateBytes(n, ""); got != maxNodeBytes {
		t.Fatalf("fixture node costs %d B, want %d: estimateBytes changed shape", got, maxNodeBytes)
	}

	// At that size the two ceilings are reached together — the node ceiling is
	// exactly reachable, and the byte ceiling still bounds a graph of larger nodes.
	if fits := hardCapBytes / maxNodeBytes; fits != hardCapNodes {
		t.Errorf("the byte ceiling admits %d nodes of %d B but the node ceiling is %d; the "+
			"smaller of the two is the only live bound and the other is decoration",
			fits, maxNodeBytes, hardCapNodes)
	}

	// At the size real nodes actually are, the NODE ceiling binds first. This is
	// the direction that matters for an in-process caller sizing a node budget:
	// it gets the budget it asked for, not a byte-derived fraction of it.
	const measuredAvgNodeBytes = 150 // avg over this repo's 26,826-node index
	avg := Node{Name: strings.Repeat("x", measuredAvgNodeBytes-50)}
	if got := estimateBytes(avg, ""); got != measuredAvgNodeBytes {
		t.Fatalf("fixture node costs %d B, want %d", got, measuredAvgNodeBytes)
	}
	if fits := hardCapBytes / measuredAvgNodeBytes; fits <= hardCapNodes {
		t.Errorf("at the measured average node size the byte ceiling stops the walk at %d "+
			"nodes, before the node ceiling at %d: a caller's node budget is again being "+
			"overruled by a byte ceiling", fits, hardCapNodes)
	}
}

// TestClampToolBytes_KeepsTheAdvertisedCeiling is ClampToolNodes' counterpart
// for the other advertised bound. Both tools apply both clamps now; a clamp
// applied to one ceiling and not the other is the half-fix this card is about.
func TestClampToolBytes_KeepsTheAdvertisedCeiling(t *testing.T) {
	if got := ClampToolBytes(math.MaxInt32); got != toolCapBytes {
		t.Errorf("an over-cap tool argument resolved to %d, want %d", got, toolCapBytes)
	}
	if got := ClampToolBytes(1234); got != 1234 {
		t.Errorf("an under-cap tool argument was altered: got %d, want 1234", got)
	}
	if got := ClampToolBytes(0); got != 0 {
		t.Errorf("an unspecified tool argument must stay 0 so the traversal default "+
			"applies; got %d", got)
	}
	if toolCapBytes >= hardCapBytes {
		t.Errorf("the tool-argument byte ceiling (%d) is not below the traversal ceiling (%d), "+
			"so an in-process caller has no headroom the tools do not also get",
			toolCapBytes, hardCapBytes)
	}
}
