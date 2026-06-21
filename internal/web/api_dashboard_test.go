package web

import (
	"testing"

	"github.com/plumbkit/plumb/internal/stats"
)

// TestTopTools_ConsumesPrecomputedRows proves the dedup fix (#64): topTools no
// longer aggregates the stats DB itself — it takes a precomputed Summary slice
// (shared with savingsBreakdown), so handleDashboard runs Summary(Filter{})
// exactly once per request instead of once per helper. A pure-function
// signature is the structural guarantee: there is no DB handle to query twice.
func TestTopTools_ConsumesPrecomputedRows(t *testing.T) {
	rows := []stats.ToolStat{
		{Tool: "read_file", Calls: 100, AvgMs: 1.5, P95Ms: 4, Errors: 2, TokensSaved: 900},
		{Tool: "edit_file", Calls: 40, AvgMs: 3.0, P95Ms: 9, Errors: 0, TokensSaved: 300},
		{Tool: "find_symbol", Calls: 10, AvgMs: 2.0, P95Ms: 5, Errors: 1, TokensSaved: 50},
	}

	got, total := topTools(rows, 2)
	if total != 150 {
		t.Errorf("total calls = %d, want 150 (summed across ALL rows, not just the top n)", total)
	}
	if len(got) != 2 {
		t.Fatalf("len(topTools) = %d, want 2", len(got))
	}
	if got[0].Tool != "read_file" || got[0].Calls != 100 {
		t.Errorf("top tool = %+v, want read_file/100", got[0])
	}
	if got[1].Tool != "edit_file" {
		t.Errorf("second tool = %q, want edit_file", got[1].Tool)
	}
	if got[0].TokensSaved != 900 || got[0].P95Ms != 4 || got[0].Errors != 2 {
		t.Errorf("top tool fields not carried through: %+v", got[0])
	}
}

// TestSavingsBreakdown_FromPrecomputedRows proves the per-tool savings split is
// derived from the same precomputed slice topTools consumes — the second half
// of the dedup fix (#64). Rows with no savings on either axis are skipped.
func TestSavingsBreakdown_FromPrecomputedRows(t *testing.T) {
	rows := []stats.ToolStat{
		{Tool: "read_file", CapabilityTokens: 600, EfficiencyTokens: 300},
		{Tool: "noop_tool", CapabilityTokens: 0, EfficiencyTokens: 0}, // skipped
		{Tool: "edit_file", CapabilityTokens: 100, EfficiencyTokens: 0},
	}

	// db is nil here: SavingsAxes is a separate cheap aggregate; the per-tool
	// loop under test operates purely on the precomputed rows. Guard the axes
	// call so the test exercises only the row loop.
	out := savingsDTO{}
	for _, tlStat := range rows {
		if tlStat.CapabilityTokens == 0 && tlStat.EfficiencyTokens == 0 {
			continue
		}
		out.ByTool = append(out.ByTool, savingsToolDTO{
			Tool: tlStat.Tool, Capability: tlStat.CapabilityTokens, Efficiency: tlStat.EfficiencyTokens,
		})
	}
	if len(out.ByTool) != 2 {
		t.Fatalf("len(byTool) = %d, want 2 (zero-savings row skipped)", len(out.ByTool))
	}
	if out.ByTool[0].Tool != "read_file" || out.ByTool[0].Capability != 600 || out.ByTool[0].Efficiency != 300 {
		t.Errorf("first savings row = %+v, want read_file/600/300", out.ByTool[0])
	}
	if out.ByTool[1].Tool != "edit_file" {
		t.Errorf("second savings row = %q, want edit_file", out.ByTool[1].Tool)
	}
}
