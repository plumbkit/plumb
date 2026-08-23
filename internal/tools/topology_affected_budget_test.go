package tools

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
	"github.com/plumbkit/plumb/internal/topology"
	goext "github.com/plumbkit/plumb/internal/topology/extractors/golang"
)

// graphCutMarker is the phrase a caller has to see when the dependent-discovery
// walk ran out of budget. Duplicated here rather than referenced, so a change to
// the user-visible wording fails a test instead of passing silently.
const graphCutMarker = "dependent discovery was cut at its traversal budget"

// TestTopologyAffected_ReportsATraversalCut drives the tool over a graph wider
// than its node budget and proves the cut is announced.
//
// This is the half of PLAN-407 that raising the budget does not fix. A budget
// that drops part of the frontier and says nothing is the original defect; the
// BFS reports Truncated, and until this test the tool discarded it, so a caller
// read a short tests-to-run list as a complete one. topology_affected is
// recall-biased by design, which is exactly why a silent cut is the expensive
// direction.
func TestTopologyAffected_ReportsATraversalCut(t *testing.T) {
	out := affectedOverStarGraph(t, graphNodeBudget+50)
	if !strings.Contains(out, graphCutMarker) {
		t.Errorf("a traversal cut must be announced; response carried no notice:\n%s", trimForFailure(out))
	}
	// The cut must be attributed to the traversal, not to max_results. Naming the
	// wrong cause sends the caller to a knob that cannot widen the walk and leaves
	// them believing the widened answer is complete.
	if strings.Contains(out, "max_results reached") {
		t.Errorf("a traversal cut was reported as a max_results cut:\n%s", trimForFailure(out))
	}
	if !strings.Contains(out, "raising max_results will not recover them") {
		t.Errorf("the notice must say max_results is not the remedy:\n%s", trimForFailure(out))
	}
}

// TestTopologyAffected_StaysSilentWhenNothingWasCut is the other direction, in
// the same build: a notice that is always printed carries no information, and a
// caller who learns to ignore it is worse off than one who never saw it.
func TestTopologyAffected_StaysSilentWhenNothingWasCut(t *testing.T) {
	out := affectedOverStarGraph(t, 20)
	if strings.Contains(out, graphCutMarker) {
		t.Errorf("a complete answer must not claim a traversal cut:\n%s", trimForFailure(out))
	}
	if strings.Contains(out, "dependent discovery hit its traversal budget") {
		t.Errorf("a complete answer must not carry the trailing cut line:\n%s", trimForFailure(out))
	}
}

func trimForFailure(s string) string {
	if len(s) > 1500 {
		return s[:1500] + "\n…"
	}
	return s
}

// affectedOverStarGraph indexes a package where n functions call Centre and one
// test sits beside them, then runs topology_affected on Centre. The star is the
// shape that makes a node budget observable: every caller is one hop out, so the
// walk is bounded by the budget and nothing else.
func affectedOverStarGraph(t *testing.T, n int) string {
	t.Helper()
	ws := t.TempDir()
	var src strings.Builder
	src.WriteString("package star\n\nfunc Centre() {}\n")
	for i := range n {
		fmt.Fprintf(&src, "\nfunc Caller%d() { Centre() }\n", i)
	}
	if err := os.WriteFile(filepath.Join(ws, "star.go"), []byte(src.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "star_test.go"),
		[]byte("package star\n\nimport \"testing\"\n\nfunc TestStar(t *testing.T) { Centre() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 4 << 20},
		[]topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		nodes, _ := s.SymbolsInFile(context.Background(), filepath.Join(ws, "star.go"))
		tests, _ := s.SymbolsInFile(context.Background(), filepath.Join(ws, "star_test.go"))
		if len(nodes) > n && len(tests) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	tool := NewTopologyAffected(func() *topology.Store { return s })
	args, _ := json.Marshal(map[string]any{"symbols": []string{"Centre"}})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return out
}
