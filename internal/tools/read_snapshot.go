package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// read_snapshot.go: the version a read records is the version the caller saw.
//
// A read records the file's mtime and SHA-256 in the session's ReadTracker, and
// the write guards compare them with the file later: an expected_mtime that
// matches but content that changed is refused (changedAtSameMtime), and so is an
// unguarded write over a file changed since the read (changedSinceSessionRead).
// Both rest on the recorded SHA being the hash of the bytes the caller was shown.
// Taking the mtime from one stat, the body from one read and the SHA from a second
// read of the path broke that: a change landing between them recorded the new
// SHA against the old body, and the guards then let the caller overwrite a change
// it never saw. read_symbol's window was the whole language-server round trip.
//
// readSnapshot reads the file once, through one descriptor, handing the bytes to
// the caller's consumer while hashing every one of them (the rest of the file is
// drained into the hash when a windowed read stops early), and takes the mtime
// from that descriptor. A writer that replaces the file (plumb's own writes,
// editors that save atomically) cannot change what the descriptor reads. One that
// rewrites it in place is caught by the descriptor's mtime and size before and
// after the read, and the read is taken again.

// snapshotAttempts bounds the re-reads of a file that keeps changing in place.
const snapshotAttempts = 3

// fileSnapshot is the version of a file a read handed to its consumer: the mtime
// its descriptor reported and the SHA-256 of exactly those bytes.
type fileSnapshot struct {
	mtime time.Time
	size  int64
	sha   string
}

// readSnapshot opens path and calls consume with a reader over its whole content,
// then returns the version consume saw. consume may stop reading early. It is
// called again, from the start, when the file changed during the read, so it must
// reset whatever it builds; an error from consume is returned as is. A file still
// changing after the last attempt keeps that attempt's hash: it may match no
// version that was ever whole on disk, but it is exactly what the caller was
// shown, so every later change still differs from it and the guards refuse.
func readSnapshot(path string, consume func(io.Reader) error) (fileSnapshot, error) {
	var last fileSnapshot
	for range snapshotAttempts {
		snap, stable, err := readSnapshotOnce(path, consume)
		if err != nil {
			return fileSnapshot{}, err
		}
		if stable {
			return snap, nil
		}
		last = snap
	}
	return last, nil
}

// snapshotLines reads path whole and splits it into lines (as fileLines does),
// returning the version those lines are.
func snapshotLines(path string) ([]string, fileSnapshot, error) {
	var lines []string
	snap, err := readSnapshot(path, func(r io.Reader) error {
		data, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		lines = strings.Split(string(data), "\n")
		return nil
	})
	return lines, snap, err
}

// stagedSnapshot is the version a write is about to publish: the SHA-256 of the
// bytes plumb wrote into its staged file, and that file's mtime. The rename that
// publishes the staged file moves its inode, mtime included, so this IS the
// target's version the moment the rename lands — known without re-reading a path
// an outside writer may already have replaced (issue #528, the write-side twin of
// the read race above).
//
// Call it after the staged file is CLOSED and before the rename: some
// filesystems (SMB, WSL's drvfs) stamp the mtime at close, so a stat of the
// still-open descriptor would trail the published mtime and the session's own
// next write would look stale. The staged name is plumb's own (a CreateTemp
// name, or the sibling under the path lock); Lstat describes exactly the entry
// the rename will move.
func stagedSnapshot(staged string, data []byte) (fileSnapshot, error) {
	info, err := os.Lstat(staged)
	if err != nil {
		return fileSnapshot{}, err
	}
	sum := sha256.Sum256(data)
	return fileSnapshot{mtime: info.ModTime(), size: int64(len(data)), sha: hex.EncodeToString(sum[:])}, nil
}

func readSnapshotOnce(path string, consume func(io.Reader) error) (fileSnapshot, bool, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path was resolved and boundary-checked by the calling tool
	if err != nil {
		return fileSnapshot{}, false, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return fileSnapshot{}, false, err
	}
	h := sha256.New()
	tee := io.TeeReader(f, h)
	if err := consume(tee); err != nil {
		return fileSnapshot{}, false, err
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return fileSnapshot{}, false, fmt.Errorf("hashing %q: %w", path, err)
	}
	after, err := f.Stat()
	if err != nil {
		return fileSnapshot{}, false, err
	}
	stable := after.ModTime().Equal(before.ModTime()) && after.Size() == before.Size()
	return fileSnapshot{mtime: before.ModTime(), size: before.Size(), sha: hex.EncodeToString(h.Sum(nil))}, stable, nil
}
