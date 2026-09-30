package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// snapshotFixture writes content to a fresh file with a known, distinct mtime.
func snapshotFixture(t *testing.T, content string, mtime time.Time) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

// replaceAtomically puts content at path the way plumb and most editors save: a
// new file renamed over the old one, with its own mtime.
func replaceAtomically(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// TestReadSnapshot_AReplacementMidReadRecordsTheBytesRead: the defect was a SHA
// taken by a second read of the path, after the body. A write landing between
// the two recorded the NEW content's SHA against the OLD body, and the write
// guards then let the caller overwrite a change it never saw. The snapshot's
// version must be the one whose bytes the consumer got.
func TestReadSnapshot_AReplacementMidReadRecordsTheBytesRead(t *testing.T) {
	oldAt, newAt := time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
	const oldContent, newContent = "old line one\nold line two\n", "NEW CONTENT\n"
	path := snapshotFixture(t, oldContent, oldAt)

	var got []byte
	snap, err := readSnapshot(path, func(r io.Reader) error {
		head := make([]byte, 4)
		if _, err := io.ReadFull(r, head); err != nil {
			return err
		}
		replaceAtomically(t, path, newContent, newAt) // a peer saves mid-read
		rest, err := io.ReadAll(r)
		got = slices.Concat(head, rest)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != oldContent {
		t.Fatalf("the consumer read %q; the open descriptor must keep the old file", got)
	}
	if snap.sha != sha256Hex([]byte(oldContent)) {
		t.Errorf("recorded sha %s is not the hash of the bytes read (%s); the new file's is %s",
			snap.sha, sha256Hex([]byte(oldContent)), sha256Hex([]byte(newContent)))
	}
	if !snap.mtime.Equal(oldAt) {
		t.Errorf("recorded mtime %v is not the read version's %v", snap.mtime, oldAt)
	}
}

// TestReadSnapshot_AnInPlaceRewriteMidReadIsReadAgain: a writer that truncates and
// rewrites the same file changes what the open descriptor reads, so the version
// read is torn. The descriptor's mtime and size show it, and the read is taken
// again; the result is the new version, whole.
func TestReadSnapshot_AnInPlaceRewriteMidReadIsReadAgain(t *testing.T) {
	path := snapshotFixture(t, "first version, long enough to be read in two parts\n", time.Now().Add(-2*time.Hour))
	const second = "second version\n"
	calls := 0
	var got []byte
	snap, err := readSnapshot(path, func(r io.Reader) error {
		calls++
		head := make([]byte, 6)
		if _, err := io.ReadFull(r, head); err != nil {
			return err
		}
		if calls == 1 {
			if err := os.WriteFile(path, []byte(second), 0o644); err != nil { // same inode, truncated
				return err
			}
		}
		rest, err := io.ReadAll(r)
		got = slices.Concat(head, rest)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("consume ran %d times; a torn read must be taken again, once", calls)
	}
	if string(got) != second || snap.sha != sha256Hex([]byte(second)) {
		t.Errorf("after the retry the read and its sha must both be the new version; got %q, sha %s", got, snap.sha)
	}
}

// TestReadSnapshot_AFileThatNeverSettlesKeepsTheLastReadsHash: when every attempt
// is torn, the last attempt's hash is kept. It is exactly what the caller was
// shown, so every later change differs from it and the guards refuse; recording
// no hash would leave them only the mtime.
func TestReadSnapshot_AFileThatNeverSettlesKeepsTheLastReadsHash(t *testing.T) {
	path := snapshotFixture(t, "v0\n", time.Now().Add(-time.Hour))
	n := 0
	var lastRead []byte
	snap, err := readSnapshot(path, func(r io.Reader) error {
		n++
		b, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		lastRead = b
		return os.WriteFile(path, []byte(string(rune('a'+n))+" grows every time\n"), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != snapshotAttempts {
		t.Fatalf("consume ran %d times, want every attempt (%d)", n, snapshotAttempts)
	}
	if snap.sha != sha256Hex(lastRead) {
		t.Errorf("the sha must be the hash of what the last attempt read (%s); got %q", sha256Hex(lastRead), snap.sha)
	}
}

// TestReadSnapshot_AWindowedReadStillHashesTheWholeFile: a consumer that stops
// early (a line window) still records the whole file's SHA, which the guards
// compare with the whole file.
func TestReadSnapshot_AWindowedReadStillHashesTheWholeFile(t *testing.T) {
	const content = "line 1\nline 2\nline 3\n"
	path := snapshotFixture(t, content, time.Now().Add(-time.Hour))
	snap, err := readSnapshot(path, func(r io.Reader) error {
		_, err := io.ReadFull(r, make([]byte, 3))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if snap.sha != sha256Hex([]byte(content)) || snap.size != int64(len(content)) {
		t.Errorf("got sha %s size %d, want the whole file's %s and %d", snap.sha, snap.size, sha256Hex([]byte(content)), len(content))
	}
}

// TestReadFile_RecordsTheVersionItPrinted: end to end, the version read_file
// records is the one in its header, which is the hash of the file it returned.
func TestReadFile_RecordsTheVersionItPrinted(t *testing.T) {
	const content = "alpha\nbeta\ngamma\n"
	path := snapshotFixture(t, content, time.Now().Add(-time.Hour))
	tracker := NewReadTracker()
	out, err := NewReadFile(tracker).Execute(t.Context(), mustJSON(map[string]any{"file_path": path, "start_line": 2, "end_line": 2}))
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := tracker.recorded(path)
	if !ok || entry.sha != sha256Hex([]byte(content)) {
		t.Fatalf("recorded %+v, want the whole file's sha %s", entry, sha256Hex([]byte(content)))
	}
	if want := "sha256=" + entry.sha; !strings.Contains(out, want) {
		t.Errorf("the header must carry the recorded sha (%s); got:\n%s", want, out)
	}
}
