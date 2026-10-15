package tools

// The context report (internal/stats, `plumb stats --context`) reads text these
// tools write. Each test here renders the producer's real output and checks the
// report's reader against it, so a format change fails here, not silently in a
// report that starts counting zeros.

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/plumbkit/plumb/internal/stats"
)

func TestReportFormats_PackFilesAndProposedSeeds(t *testing.T) {
	s := newShop(t)
	args := map[string]any{"task": "the checkout charges the ledger twice", "intent": "change"}
	out, err := s.run(t, args)
	if err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(args)
	files, seeds, proposed, prose := stats.PackFilesForReport(string(in), out)
	if !prose {
		t.Error("a prose-only request must read as prose")
	}
	if !slices.Contains(seeds, "api/checkout.go") || !slices.Contains(proposed, "api/checkout.go") {
		t.Errorf("seeds %v / proposed %v must include api/checkout.go:\n%s", seeds, proposed, out)
	}
	if len(files) <= len(seeds) {
		t.Errorf("files %v should add the related declarations' files to the seeds %v:\n%s", files, seeds, out)
	}
	// The top-ranked related declaration always renders; its file must be read.
	pack := s.collect(t, args)
	for _, r := range pack.Related {
		if r.Gap {
			continue
		}
		if !slices.Contains(files, r.Node.Path) {
			t.Errorf("top related file %s is missing from the report's files %v", r.Node.Path, files)
		}
		break
	}

	explicit, _ := json.Marshal(map[string]any{"files": []string{"store/ledger.go"}})
	out2, err := s.tool.Execute(context.Background(), explicit)
	if err != nil {
		t.Fatal(err)
	}
	_, seeds2, proposed2, prose2 := stats.PackFilesForReport(string(explicit), out2)
	if prose2 || len(proposed2) != 0 || !slices.Equal(seeds2, []string{"store/ledger.go"}) {
		t.Errorf("explicit pack read as prose=%v proposed=%v seeds=%v", prose2, proposed2, seeds2)
	}
}

func TestReportFormats_DiagnosticsDelta(t *testing.T) {
	fresh := diagDelta{
		Fresh: true, Scopes: diagScopes{EditedFile: diagScopeFresh, CrossFile: diagScopeIncomplete},
		NewErrors:          []diagEntry{{File: "a.go", Line: 1, Severity: "error", Message: "x"}, {File: "a.go", Line: 2, Severity: "warning", Message: "y"}},
		CrossFileNewErrors: []diagEntry{{File: "b.go", Line: 3, Severity: "error", Message: "z"}},
	}
	if present, n := stats.EditNewErrorsForReport("applied 1 edit" + fresh.line()); !present || n != 2 {
		t.Errorf("fresh delta read as present=%v n=%d, want true 2 (two errors, the warning not counted)", present, n)
	}
	clean := diagDelta{Fresh: true, Scopes: diagScopes{EditedFile: diagScopeFresh}}
	if present, n := stats.EditNewErrorsForReport("applied" + clean.line()); !present || n != 0 {
		t.Errorf("clean delta read as present=%v n=%d", present, n)
	}
	if present, _ := stats.EditNewErrorsForReport("applied" + unconfirmedDelta(diagScopeNotChecked).line()); present {
		t.Error("an unconfirmed delta is not a diagnosis")
	}
}

func TestReportFormats_RunTaskOutcome(t *testing.T) {
	root := t.TempDir()
	run := func(step string) string {
		tool := NewTasks(WriteDeps{WorkspaceFn: func(context.Context) string { return root }},
			func(_ context.Context, req TaskRequest) (TaskCommand, error) {
				return TaskCommand{Slot: req.Slot, Provenance: "project", Root: root, Steps: [][]string{{"sh", "-c", step}}}, nil
			})
		out, err := tool.Execute(context.Background(), json.RawMessage(`{"slot":"test"}`))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := stats.TestOutcomeForReport(`{"slot":"test"}`, run("true")); got != "pass" {
		t.Errorf("a passing test run reads as %q", got)
	}
	if got := stats.TestOutcomeForReport(`{"slot":"test"}`, run("echo '→ ok'; exit 1")); got != "fail" {
		t.Errorf("a failing test run (whose own output says → ok) reads as %q", got)
	}
	if got := stats.TestOutcomeForReport(`{"slot":"build"}`, run("true")); got != "" {
		t.Errorf("a build run is not a test outcome; got %q", got)
	}
}
