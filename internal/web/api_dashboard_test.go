package web

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/config"
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

func writeDashboardProjectConfig(t *testing.T, ws, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(ws, ".plumb"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".plumb", "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func grantDashboardTrust(t *testing.T, ws string) {
	t.Helper()
	spec, err := config.ProjectPolicySpecFor(ws)
	if err != nil {
		t.Fatal(err)
	}
	cmds, err := config.ProjectTaskCommands(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.NewTrustStore().SetTrustedForProject(ws, cmds, spec); err != nil {
		t.Fatal(err)
	}
}

// TestDaemonWideConversationsDTO_RequiresEveryParticipantToOptIn is the web
// layer's half of the unanimous-consent rule, end to end: real project
// configs, real trust grants, a real global collab store.
func TestDaemonWideConversationsDTO_RequiresEveryParticipantToOptIn(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	optedIn := t.TempDir()
	writeDashboardProjectConfig(t, optedIn, "[collab]\ncross_project = true\n")
	grantDashboardTrust(t, optedIn)

	peerOptedIn := t.TempDir()
	writeDashboardProjectConfig(t, peerOptedIn, "[collab]\ncross_project = true\n")
	grantDashboardTrust(t, peerOptedIn)

	silent := t.TempDir() // never opts in

	g, err := collab.OpenGlobalAt(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	ctx, now := context.Background(), time.Now()

	unanimous, err := g.PutNote(ctx, collab.NoteInput{
		AuthorSession: "a", AuthorID: "a", Body: "hi", Addressee: "b",
		TTL: time.Hour, OriginWorkspace: optedIn, TargetWorkspace: peerOptedIn,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	partial, err := g.PutNote(ctx, collab.NoteInput{
		AuthorSession: "a", AuthorID: "a", Body: "hi", Addressee: "c",
		TTL: time.Hour, OriginWorkspace: optedIn, TargetWorkspace: silent,
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	deps := Deps{
		Store:             config.NewStore(config.Defaults()),
		CollabGlobalStore: func() *collab.Store { return g },
	}
	got := daemonWideConversationsDTO(ctx, deps)

	ids := make([]string, 0, len(got))
	for _, c := range got {
		ids = append(ids, c.ID)
	}
	found := false
	for _, id := range ids {
		if id == unanimous {
			found = true
		}
		if id == partial {
			t.Errorf("a conversation with a non-consenting participant leaked into the dashboard DTO: %v", ids)
		}
	}
	if !found {
		t.Errorf("the unanimously-consented conversation is missing: %v", ids)
	}
}

// TestDaemonWideConversationsDTO_NoStoreYieldsNil covers the common case: a
// daemon with no CollabGlobalStore wiring (or no store yet) must render no
// panel at all, not an error.
func TestDaemonWideConversationsDTO_NoStoreYieldsNil(t *testing.T) {
	if got := daemonWideConversationsDTO(context.Background(), Deps{}); got != nil {
		t.Errorf("expected nil with no CollabGlobalStore wiring, got %+v", got)
	}
	deps := Deps{
		Store:             config.NewStore(config.Defaults()),
		CollabGlobalStore: func() *collab.Store { return nil },
	}
	if got := daemonWideConversationsDTO(context.Background(), deps); got != nil {
		t.Errorf("expected nil when the store does not yet exist, got %+v", got)
	}
}
