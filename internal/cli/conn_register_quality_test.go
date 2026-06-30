package cli

import (
	"context"
	"testing"
)

// TestBuildWriteDeps_QualityReportIsLazy verifies WriteDeps.QualityReport is a
// lazy closure that resolves the quality runner per-write, not an eager capture
// taken at tool-registration time (before attach), which was always nil and
// permanently disabled the [quality] post-write findings. Regression for
// quality-1.
func TestBuildWriteDeps_QualityReportIsLazy(t *testing.T) {
	s := &connSession{}
	s.state.Store(&sessionView{})

	deps := s.buildWriteDeps()
	if deps.QualityReport == nil {
		t.Fatal("QualityReport must be a lazy closure, never nil — the eager capture made it nil before attach")
	}
	// With no runner attached yet it must be a safe no-op, not a panic.
	if got := deps.QualityReport(context.Background(), "x.go"); got != "" {
		t.Errorf("no-runner report = %q, want empty", got)
	}
}
