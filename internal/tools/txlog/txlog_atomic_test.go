package txlog

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestWriteManifest_AtomicUnderConcurrentScan exercises the torn-read window the
// atomic writeManifest closes: a live multi-file transaction rewrites its manifest
// on every Record, while a cross-connection Scan reads tx-log manifests for orphan
// recovery. If the manifest were written non-atomically (truncate-in-place), a Scan
// could read a half-written manifest, fail to parse StartedAt, miss the live-cutoff
// guard, and roll back the live transaction — reverting its in-progress file and
// deleting its log. With the atomic temp+rename write a reader always sees a
// complete manifest, so the live transaction survives every concurrent Scan.
// Runs under -race. Regression test for the toolsfs-2 rework.
func TestWriteManifest_AtomicUnderConcurrentScan(t *testing.T) {
	ws := initWorkspace(t)

	// The live transaction always starts after this cutoff, so a correct Scan must
	// always skip it; only a torn manifest read would misclassify it as an orphan.
	cutoff := time.Now().Add(-time.Hour)

	target := filepath.Join(ws, "live.txt")
	if err := os.WriteFile(target, []byte("snapshot-base"), 0o644); err != nil {
		t.Fatal(err)
	}
	live, err := Begin(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := live.Record(target, []byte("snapshot-base"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The on-disk file diverges from the snapshot: a wrongful rollback would revert
	// it to "snapshot-base", which the post-check detects.
	if err := os.WriteFile(target, []byte("live-in-progress"), 0o644); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writer: rewrite the manifest repeatedly, as a live multi-file transaction's
	// successive Records would, creating torn-read windows for the readers.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = live.Record(target, []byte("snapshot-base"), 0o644)
			}
		}
	}()

	// Readers: concurrent Scans that must never roll back the live transaction.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				Scan(ws, cutoff)
			}
		}()
	}

	// Let the readers run, then stop the writer.
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()

	if _, err := os.Stat(live.dir); err != nil {
		t.Errorf("live tx-log dir was wrongly removed by a concurrent Scan: %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "live-in-progress" {
		t.Errorf("a concurrent Scan reverted the live transaction's file: %q", got)
	}
}
