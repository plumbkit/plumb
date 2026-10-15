package cli

import (
	"bytes"
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
