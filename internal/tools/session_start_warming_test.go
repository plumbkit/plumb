package tools

import (
	"strings"
	"testing"
	"time"
)

func TestSessionStart_WarmingAdvisory(t *testing.T) {
	ss := (&SessionStart{}).WithLSPWarmup(func() (bool, time.Duration) { return true, 4 * time.Second })
	var sb strings.Builder
	if !ss.writeLSPWarming(&sb) {
		t.Fatal("expected the warming advisory to be written")
	}
	out := sb.String()
	if !strings.Contains(out, "warming up") || !strings.Contains(out, "topology_search") {
		t.Fatalf("unexpected advisory: %q", out)
	}
	if !strings.Contains(out, "~4s elapsed") {
		t.Fatalf("expected elapsed time in advisory: %q", out)
	}

	noWarm := &SessionStart{}
	var sb2 strings.Builder
	if noWarm.writeLSPWarming(&sb2) || sb2.Len() != 0 {
		t.Fatal("expected no advisory when no accessor is wired")
	}
}

// TestSessionStart_WarmingBeatsAvailable verifies the warming advisory pre-empts
// the "LSP is available" line — an attached-but-cold server must not be reported
// as ready.
func TestSessionStart_WarmingBeatsAvailable(t *testing.T) {
	ss := (&SessionStart{}).
		WithLSPLanguage(func() string { return "go" }).
		WithLSPWarmup(func() (bool, time.Duration) { return true, 2 * time.Second })
	var sb strings.Builder
	ss.writeSessionRecommendedStart(&sb, false, "Go", "go")
	out := sb.String()
	if strings.Contains(out, "LSP is available") {
		t.Fatalf("warming should pre-empt 'LSP is available': %q", out)
	}
	if !strings.Contains(out, "warming up") {
		t.Fatalf("expected warming advisory: %q", out)
	}
}
