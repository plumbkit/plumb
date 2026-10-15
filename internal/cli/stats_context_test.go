package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/stats"
)

func TestCheckContextFlagCompat(t *testing.T) {
	cmd := &cobra.Command{Use: "stats"}
	cmd.Flags().Bool("health", false, "")
	cmd.Flags().Bool("failures", false, "")
	cmd.Flags().Int("limit", 20, "")
	reset := func() { statsFlagContext, statsFlagJSON, statsFlagAllWorkspaces = false, false, false }
	t.Cleanup(reset)

	statsFlagJSON = true
	if err := checkContextFlagCompat(cmd); err == nil {
		t.Error("--json without --context must be refused")
	}
	reset()
	statsFlagAllWorkspaces = true
	if err := checkContextFlagCompat(cmd); err == nil {
		t.Error("--all-workspaces without --context must be refused")
	}
	reset()
	statsFlagContext = true
	if err := checkContextFlagCompat(cmd); err != nil {
		t.Errorf("--context alone: %v", err)
	}
	_ = cmd.Flags().Set("failures", "true")
	if err := checkContextFlagCompat(cmd); err == nil || !strings.Contains(err.Error(), "--failures") {
		t.Errorf("--context with --failures: %v", err)
	}
}

func TestRenderContextReport(t *testing.T) {
	wk := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	p := stats.ContextPeriod{
		Start: wk, Streams: 4, StreamsWithPack: 1, Packs: 2, ProsePacks: 1,
		EditedFiles: 4, EditedFilesInPack: 3, PackFiles: 10, PackFilesUsed: 5,
		WithPack:    stats.ContextOutcome{Streams: 1, Edits: 6, EditFailures: 1, TestRuns: 2, TestFailures: 1, StreamsWithTests: 1, EndedGreen: 1, FilesEdited: 3},
		WithoutPack: stats.ContextOutcome{Streams: 2, Edits: 4, FilesEdited: 4},
	}
	r := stats.ContextReport{
		Since: wk, Until: wk.Add(7 * 24 * time.Hour), Weeks: []stats.ContextPeriod{p}, Total: p,
		Leads: stats.ContextLeads{
			MissedFiles: []stats.ContextCount{{Text: "/w/store/ledger.go", Count: 2}},
			Feedback:    []stats.ContextFeedback{{At: wk, Tool: "context_for_task", Missing: "callers"}},
		},
	}
	var out bytes.Buffer
	renderContextReport(&out, "/w", r)
	got := out.String()
	for _, want := range []string{
		"wk 10-05", "total", "3/4 (75%)", "5/10 (50%)", "2 (1)", "1/6 (17%) | 0/4 (0%)",
		"     2  store/ledger.go", "context_for_task: missing: callers", "Only plumb calls are recorded",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report does not contain %q:\n%s", want, got)
		}
	}
	out.Reset()
	renderContextReport(&out, "/w", stats.ContextReport{Since: wk, Until: wk})
	if !strings.Contains(out.String(), "No plumb calls recorded") {
		t.Errorf("empty report: %s", out.String())
	}
}

// --json prints the report as JSON (no logo, so a pipe can read it), with
// lists that are never null, whether or not the stats database exists yet.
func TestRunStatsContext_JSON(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	statsFlagJSON = true
	t.Cleanup(func() { statsFlagJSON = false })
	var out bytes.Buffer
	if err := runStatsContext("/w", &out); err != nil {
		t.Fatal(err)
	}
	var empty map[string]any
	if err := json.Unmarshal(out.Bytes(), &empty); err != nil || strings.Contains(out.String(), "null") {
		t.Fatalf("no database: not JSON, or a null list: %v\n%s", err, out.String())
	}

	db, err := stats.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Record(stats.Call{
		Workspace: "/w", SessionID: "s", Tool: "context_for_task", CalledAt: time.Now(),
		InputJSON: `{"task":"cart total"}`, OutputText: "context_for_task root=/w\nseeds (0 resolved, 0 unresolved):\n", Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	out.Reset()
	if err := runStatsContext("/w", &out); err != nil {
		t.Fatal(err)
	}
	var r stats.ContextReport
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatalf("not a report: %v\n%s", err, out.String())
	}
	if r.Total.Packs != 1 || r.Total.ProsePacks != 1 {
		t.Errorf("report = %+v, want one prose pack", r.Total)
	}
}

func TestCheckContextFlagCompat_WorkspaceAndAll(t *testing.T) {
	cmd := &cobra.Command{Use: "stats"}
	for _, f := range []string{"health", "failures"} {
		cmd.Flags().Bool(f, false, "")
	}
	cmd.Flags().Int("limit", 20, "")
	cmd.Flags().String("workspace", "", "")
	statsFlagContext, statsFlagAllWorkspaces = true, true
	t.Cleanup(func() { statsFlagContext, statsFlagAllWorkspaces = false, false })
	if err := checkContextFlagCompat(cmd); err != nil {
		t.Fatalf("--all-workspaces alone: %v", err)
	}
	_ = cmd.Flags().Set("workspace", "/w")
	if err := checkContextFlagCompat(cmd); err == nil || !strings.Contains(err.Error(), "alternatives") {
		t.Errorf("--all-workspaces with --workspace: %v", err)
	}
}
