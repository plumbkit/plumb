package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/contexthints"
	"github.com/plumbkit/plumb/internal/stats"
)

// TestHooksUptake_FirstPromptHintOnAnUnsharedConnectionIsConsumed drives the real
// `plumb hooks uptake` over both real stores, in the shape the live Codex
// dogfood recorded (PLAN-511): a hint on the session's first prompt, before its
// first plumb call, then a session_start declaring that session and a read of
// the hinted file. The connection is unshared, so none of its calls carries a
// logical_agent; the hint must still read as consumed, through the connection
// its session alone declared. The decoy hint, for a session that declared
// nothing, is the control: it stays unattributed.
func TestHooksUptake_FirstPromptHintOnAnUnsharedConnectionIsConsumed(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	hint := time.Now().Add(-5 * time.Minute).Truncate(time.Millisecond)

	ledger, err := contexthints.Open()
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	for _, sid := range []string{"codex-conv", "silent-conv"} {
		if err := ledger.Record(contexthints.Observation{
			At: hint, Workspace: "/w", SessionID: sid, Host: "codex", Event: "UserPromptSubmit",
			Emitted: []string{"internal/topology/indexer_calls.go"}, EmittedBytes: 955,
			Outcome: contexthints.OutcomeEmitted, Detail: "cwd-root",
		}); err != nil {
			t.Fatalf("record observation: %v", err)
		}
	}
	ledger.Close()

	sdb, err := stats.Open()
	if err != nil {
		t.Fatalf("open stats: %v", err)
	}
	for i, c := range []stats.Call{
		{SessionID: "conn-1", Tool: "session_start", InputJSON: `{"detail":"brief","session_id":"codex-conv"}`},
		{SessionID: "conn-1", Tool: "read_symbol", InputJSON: `{"path":"internal/topology/indexer_calls.go","name":"samePackageOutcome"}`},
		{SessionID: "conn-2", Tool: "read_symbol", InputJSON: `{"path":"internal/topology/indexer_calls.go","name":"x"}`},
	} {
		c.Workspace, c.Success, c.CalledAt = "/w", true, hint.Add(time.Duration(i+1)*10*time.Second)
		if err := sdb.Record(c); err != nil {
			t.Fatalf("record call: %v", err)
		}
	}
	sdb.Close()

	prevJSON := hooksUptakeJSON
	hooksUptakeJSON = true
	t.Cleanup(func() { hooksUptakeJSON = prevJSON })
	outPath := filepath.Join(t.TempDir(), "uptake.json")
	out, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := runHooksUptake(out); err != nil {
		t.Fatalf("runHooksUptake: %v", err)
	}
	out.Close()
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Hosts []contexthints.HostUptake `json:"hosts"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, raw)
	}
	if len(report.Hosts) != 1 {
		t.Fatalf("want one host line, got %+v", report.Hosts)
	}
	h := report.Hosts[0]
	// conn-2 read the hinted file too, but no session declared it: it is
	// evidence about nobody, so silent-conv's hint stays unattributed.
	if h.Consumed != 1 || h.Unconsumed != 0 || h.Unattributed != 1 {
		t.Errorf("codex consumption = consumed %d, not consumed %d, unattributed %d; want 1, 0, 1",
			h.Consumed, h.Unconsumed, h.Unattributed)
	}
}
