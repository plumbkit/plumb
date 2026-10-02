package txlog

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// crashLeaving snapshots target's "orig\n" under a log owned by callID, then
// overwrites target with current and abandons the log, as a crash would.
func crashLeaving(t *testing.T, callID string, current []byte) (ws, target string) {
	t.Helper()
	ws = t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, ".plumb"), 0o755); err != nil {
		t.Fatal(err)
	}
	target = filepath.Join(ws, "f.txt")
	if err := os.WriteFile(target, []byte("orig\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := Begin(ws, callID)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Record(target, []byte("orig\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, current, 0o644); err != nil {
		t.Fatal(err)
	}
	return ws, target
}

func scanAll(ws string) []Restored {
	var got []Restored
	ScanRecording(ws, time.Now().Add(time.Hour), func(string) error { return nil }, func(r Restored) { got = append(got, r) })
	return got
}

func TestScanRecordingReportsEachRestoreWithTheManifestCallID(t *testing.T) {
	ws, _ := crashLeaving(t, "01CALLID", []byte("half-written\n"))
	got := scanAll(ws)
	if len(got) != 1 || got[0].CallID != "01CALLID" || string(got[0].Before.Content) != "half-written\n" || string(got[0].After) != "orig\n" {
		t.Fatalf("restores = %+v", got)
	}
}

// A manifest written before call_id linking (omitempty: no field at all) must
// still replay, reporting an empty CallID.
func TestManifestWithoutCallIDStillReplays(t *testing.T) {
	ws, target := crashLeaving(t, "", []byte("half-written\n"))
	entries, err := os.ReadDir(filepath.Join(ws, txLogSubDir))
	if err != nil || len(entries) != 1 {
		t.Fatalf("log dirs = %v, %v", entries, err)
	}
	manifest, err := os.ReadFile(filepath.Join(ws, txLogSubDir, entries[0].Name(), "manifest.json"))
	if err != nil || strings.Contains(string(manifest), "call_id") {
		t.Fatalf("control: the manifest must carry no call_id field: %s, %v", manifest, err)
	}
	got := scanAll(ws)
	if len(got) != 1 || got[0].CallID != "" {
		t.Fatalf("restores = %+v; want one restore with no call id", got)
	}
	if b, _ := os.ReadFile(target); string(b) != "orig\n" {
		t.Fatalf("file not restored: %q", b)
	}
}

// A file too large to carry is hashed by streaming — never recorded as an
// existing EMPTY file, which is what it used to read as past the snapshot cap.
func TestRecoveryOfAnOversizedFileIsHashedNotEmptied(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 11<<20)
	ws, _ := crashLeaving(t, "01BIG", big)
	got := scanAll(ws)
	if len(got) != 1 {
		t.Fatalf("restores = %d", len(got))
	}
	b := got[0].Before
	want := sha256.Sum256(big)
	if !b.Exists || b.Content != nil || b.Size != int64(len(big)) || !bytes.Equal(b.SHA, want[:]) {
		t.Fatalf("before = exists %v, carried %d bytes, size %d, sha match %v; want the 11 MiB file hashed, not carried",
			b.Exists, len(b.Content), b.Size, bytes.Equal(b.SHA, want[:]))
	}
}
