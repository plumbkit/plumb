package tools

import (
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/stats"
	"github.com/plumbkit/plumb/internal/toolerror"
)

// TestWriteSessionStats_ThreeHonestLines is the PLAN-367 acceptance check:
// with a surcharge accessor wired, a v4-scored savings row, and a
// guard-classified failure recorded, the banner shows all three honest
// economics lines — surcharge, netted read savings (current model version
// only), and guard refusals — with plausible, non-fabricated values.
func TestWriteSessionStats_ThreeHonestLines(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	db, err := stats.Open()
	if err != nil {
		t.Fatalf("stats.Open: %v", err)
	}
	defer db.Close()

	now := time.Now()
	// A tool-usage row so the "Most-used tools" header (and this function)
	// actually renders anything.
	if err := db.Record(stats.Call{
		SessionID: "s", Workspace: "/ws", Tool: "read_symbol",
		CalledAt: now, DurationMs: 50, Success: true,
	}); err != nil {
		t.Fatalf("Record usage: %v", err)
	}
	// A v4-scored savings row.
	if err := db.Record(stats.Call{
		SessionID: "s", Workspace: "/ws", Tool: "read_symbol",
		CalledAt: now, Success: true,
		SavingsModelVersion: 4, CapabilityTokens: 0, EfficiencyTokens: 900, TokensSaved: 900,
	}); err != nil {
		t.Fatalf("Record savings: %v", err)
	}
	// An older-version row that must NOT be folded into the v4 total.
	if err := db.Record(stats.Call{
		SessionID: "s", Workspace: "/ws", Tool: "read_file",
		CalledAt: now, Success: true,
		SavingsModelVersion: 3, CapabilityTokens: 0, EfficiencyTokens: 5000, TokensSaved: 5000,
	}); err != nil {
		t.Fatalf("Record v3 row: %v", err)
	}
	// A guard-classified failure so guard refusals > 0.
	if err := db.Record(stats.Call{
		SessionID: "s", Workspace: "/ws", Tool: "edit_file",
		CalledAt: now, Success: false,
		ErrorKind: toolerror.KindDirtyFile, RemediationClass: toolerror.ClassPassDirtyOk,
	}); err != nil {
		t.Fatalf("Record failure: %v", err)
	}

	// bytes/tokens mirror the PLAN-367 review-round measurement of the live
	// 59-tool payload (106,401 chars / 23,276 tokens, cl100k) — see
	// clientcaps.surchargeCharsPerToken's doc comment.
	ss := (&SessionStart{}).WithSurcharge(func() (int, int, int) { return 106401, 23276, 59 })

	var sb strings.Builder
	ss.writeSessionStats(&sb, "/ws")
	out := sb.String()

	if !strings.Contains(out, "profile surcharge: 106401 bytes") || !strings.Contains(out, "~23k tokens estimated") {
		t.Errorf("missing/wrong surcharge line (must show measured bytes AND a labelled token estimate):\n%s", out)
	}
	if !strings.Contains(out, "estimated read savings: ~900 tokens") {
		t.Errorf("read savings line missing or not scoped to v4 only (must exclude the 5000-token v3 row):\n%s", out)
	}
	if strings.Contains(out, "5.9k") {
		t.Errorf("v3 and v4 rows were summed together, violating the no-cross-version-sum rule:\n%s", out)
	}
	if !strings.Contains(out, "guard refusals: 1") {
		t.Errorf("missing/wrong guard-refusals line:\n%s", out)
	}
}
