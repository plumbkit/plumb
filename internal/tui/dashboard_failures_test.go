package tui

import (
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/stats"
	"github.com/plumbkit/plumb/internal/toolerror"
)

func TestFailuresByKind_CollapsesAndOrders(t *testing.T) {
	got := failuresByKind([]stats.FailureCount{
		{Kind: toolerror.KindDirtyFile, Tool: "edit_file", ClientVersion: "1.0", Calls: 2, Retryable: 2},
		{Kind: toolerror.KindDirtyFile, Tool: "write_file", ClientVersion: "0.9", Calls: 1, Retryable: 1},
		{Kind: toolerror.KindGitPolicy, Tool: "git", Calls: 5},
		{Tool: "git", Calls: 1},
	})
	want := []dashFailureRow{
		{label: "git_policy", calls: 5},
		{label: "dirty_file", calls: 3, retryable: 3},
		{label: stats.UnclassifiedLabel, calls: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestDashFailuresWidget_SilentWithoutFailures(t *testing.T) {
	if lines := (Model{}).dashFailuresWidget(80); lines != nil {
		t.Errorf("widget rendered with no failures:\n%s", strings.Join(lines, "\n"))
	}
}

func TestDashFailuresWidget_NamesTheKind(t *testing.T) {
	RebuildStyles()
	m := Model{dashUptimeFailures: []stats.FailureCount{
		{Kind: toolerror.KindLSPTimeout, Tool: "find_references", Calls: 4, Retryable: 4},
	}}
	out := strings.Join(m.dashFailuresWidget(80), "\n")
	for _, want := range []string{"Failures by Kind", "lsp_timeout", "Retryable"} {
		if !strings.Contains(out, want) {
			t.Errorf("widget missing %q:\n%s", want, out)
		}
	}
}
