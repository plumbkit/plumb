package txlog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestScan_SkipsCurrentRunButRollsBackOrphan verifies the liveCutoff: a tx-log
// directory started before the cutoff (a previous-run orphan) is rolled back,
// while one started at/after the cutoff (a live transaction in the current run)
// is left untouched — so a second connection's attach can never revert another
// connection's in-flight transaction. Regression test for toolsfs-2.
func TestScan_SkipsCurrentRunButRollsBackOrphan(t *testing.T) {
	ws := initWorkspace(t)

	// Orphan from a "previous run": started, snapshotted a file, then the daemon
	// "crashed" (no Commit), leaving the half-applied write on disk.
	orphanTarget := filepath.Join(ws, "orphan.txt")
	if err := os.WriteFile(orphanTarget, []byte("orig"), 0o644); err != nil {
		t.Fatal(err)
	}
	orphan, err := Begin(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := orphan.Record(orphanTarget, []byte("orig"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphanTarget, []byte("half-applied"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Cutoff sits between the orphan and the live transaction.
	time.Sleep(2 * time.Millisecond)
	cutoff := time.Now()
	time.Sleep(2 * time.Millisecond)

	// Live transaction in the "current run": started after the cutoff.
	liveTarget := filepath.Join(ws, "live.txt")
	if err := os.WriteFile(liveTarget, []byte("live-base"), 0o644); err != nil {
		t.Fatal(err)
	}
	live, err := Begin(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := live.Record(liveTarget, []byte("live-base"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(liveTarget, []byte("live-in-progress"), 0o644); err != nil {
		t.Fatal(err)
	}

	Scan(ws, cutoff)

	// Orphan rolled back: dir removed and file restored.
	if _, err := os.Stat(orphan.dir); !os.IsNotExist(err) {
		t.Error("orphan tx-log dir should have been removed")
	}
	if got, _ := os.ReadFile(orphanTarget); string(got) != "orig" {
		t.Errorf("orphan file not restored: %q", got)
	}
	// Live transaction untouched: dir survives, in-progress write kept.
	if _, err := os.Stat(live.dir); err != nil {
		t.Errorf("live tx-log dir was wrongly removed: %v", err)
	}
	if got, _ := os.ReadFile(liveTarget); string(got) != "live-in-progress" {
		t.Errorf("Scan reverted a live transaction's file: %q", got)
	}
}
