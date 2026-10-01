package session_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/session"
)

// registerEnded registers n sessions and ends them, returning their ids.
func registerEnded(t *testing.T, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for range n {
		info, err := session.Register(session.Info{Folder: "/w"})
		if err != nil {
			t.Fatal(err)
		}
		session.Unregister(info.ID)
		ids = append(ids, info.ID)
	}
	return ids
}

func listIDs(t *testing.T) map[string]bool {
	t.Helper()
	infos, err := session.List()
	if err != nil {
		t.Fatal(err)
	}
	ids := make(map[string]bool, len(infos))
	for _, in := range infos {
		ids[in.ID] = true
	}
	return ids
}

// TestList_DoesNotRereadUnchangedEndedFiles is the root cause of
// workspace_sessions' "timed out reading session or stats data" (#545). Ended
// sessions are kept for a 24-hour grace window, and a busy machine accumulates
// about a thousand of them; List opened and parsed every one on every call, under
// an EXCLUSIVE flock. At ~0.25 ms per open on macOS that is ~290 ms per List —
// serialised across every caller — against workspace_sessions' 500 ms budget, so
// the second of two concurrent callers timed out. An ended file never changes
// while it waits out its grace, so a List must not re-read it.
//
// The read counter makes this deterministic where a timing test would flake.
func TestList_DoesNotRereadUnchangedEndedFiles(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	registerEnded(t, 5)
	live, err := session.Register(session.Info{Folder: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Unregister(live.ID) })
	// One List to learn the directory. (Register lists too, so how many files
	// this reads depends on what registering already taught it.)
	if ids := listIDs(t); !ids[live.ID] || len(ids) != 1 {
		t.Fatalf("first List = %v, want only the live session %s", ids, live.ID)
	}
	reads := session.CountSessionFileReadsForTest(t)
	if ids := listIDs(t); !ids[live.ID] || len(ids) != 1 {
		t.Fatalf("second List = %v, want only the live session %s", ids, live.ID)
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("second List read %d files, want 1: only the live session's file can have changed", got)
	}
}

// TestList_RereadsAnEndedFileThatChanged is the cache's correctness half: a
// remembered ended file that is REWRITTEN (a reconnect adopting the id writes a
// fresh, live record through the atomic rename) must be read again, or a live
// session would vanish from every roster.
func TestList_RereadsAnEndedFileThatChanged(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	id := registerEnded(t, 1)[0]
	if ids := listIDs(t); ids[id] {
		t.Fatalf("an ended session was listed as live")
	}
	// Revive it in place: same id, same file name, a live record.
	session.Patch(id, func(in *session.Info) { in.EndedAt = time.Time{} })
	if ids := listIDs(t); !ids[id] {
		t.Fatalf("a session file rewritten as live was not re-read; List still treats it as ended")
	}
}

// TestList_EachIdentityFieldInvalidates rewrites a remembered ended file as a
// live record, changing exactly ONE of the three things the cache keys on. Each
// alone must force a re-read: the writers in this package change all three at
// once, but another plumb version, or a filesystem with coarse mtimes, need not.
func TestList_EachIdentityFieldInvalidates(t *testing.T) {
	cases := []struct {
		name  string
		write func(t *testing.T, path string, old []byte, oldFI os.FileInfo)
	}{
		{"inode only", func(t *testing.T, path string, old []byte, oldFI os.FileInfo) {
			t.Helper()
			tmp := path + ".tmp"
			writeFile(t, tmp, padTo(t, liveRecord(t, old), len(old)))
			if err := os.Rename(tmp, path); err != nil {
				t.Fatal(err)
			}
			setMtime(t, path, oldFI.ModTime())
		}},
		{"mtime only", func(t *testing.T, path string, old []byte, oldFI os.FileInfo) {
			t.Helper()
			writeFile(t, path, padTo(t, liveRecord(t, old), len(old))) // in place: same inode
			setMtime(t, path, oldFI.ModTime().Add(time.Second))
		}},
		{"size only", func(t *testing.T, path string, old []byte, oldFI os.FileInfo) {
			t.Helper()
			writeFile(t, path, liveRecord(t, old)) // in place, shorter
			setMtime(t, path, oldFI.ModTime())
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			id := registerEnded(t, 1)[0]
			dir, err := session.Dir()
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, id+".json")
			listIDs(t) // remembered as ended
			old, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			oldFI, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			tc.write(t, path, old, oldFI)
			if ids := listIDs(t); !ids[id] {
				t.Fatalf("a session file changed in %s was not re-read; List still treats it as ended", tc.name)
			}
		})
	}
}

// liveRecord returns the session record in old with its end cleared.
func liveRecord(t *testing.T, old []byte) []byte {
	t.Helper()
	var in session.Info
	if err := json.Unmarshal(old, &in); err != nil {
		t.Fatal(err)
	}
	in.EndedAt = time.Time{}
	out, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// padTo pads JSON with trailing spaces (still valid JSON) to exactly n bytes.
func padTo(t *testing.T, b []byte, n int) []byte {
	t.Helper()
	if len(b) > n {
		t.Fatalf("record is %d bytes, longer than the %d it must match", len(b), n)
	}
	return append(b, bytes.Repeat([]byte(" "), n-len(b))...)
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o644); err != nil { //nolint:gosec // G306: a session file fixture
		t.Fatal(err)
	}
}

func setMtime(t *testing.T, path string, mt time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
}

// TestList_StillPrunesARememberedEndedFile: remembering that a file is ended
// must not stop List deleting it once its grace has passed.
func TestList_StillPrunesARememberedEndedFile(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	id := registerEnded(t, 1)[0]
	dir, err := session.Dir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".json")
	// Ended so long ago that its grace runs out a moment from now.
	const margin = 500 * time.Millisecond
	session.Patch(id, func(in *session.Info) { in.EndedAt = time.Now().Add(-session.EndedSessionGraceForTest + margin) })
	listIDs(t) // remembered as ended, still inside its grace
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a session file inside its grace was removed: %v", err)
	}
	time.Sleep(margin + 200*time.Millisecond) // now past it; the file itself is untouched
	listIDs(t)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a remembered ended file past its grace was not pruned (stat err = %v)", err)
	}
}
