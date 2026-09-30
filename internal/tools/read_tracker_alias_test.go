package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Issue #524: the ReadTracker keyed a read on the path AS SPELLED while the
// write tracker, the undo store and the write lock key on lockPathKey. A file
// read through one spelling and written through another then had no read record
// at the write, and changedSinceSessionRead — which treats "never read" as "not
// stale" — let the write overwrite a peer's change. These tests read and write
// one file through two spellings (a symlinked parent, built by aliasedProject so
// the alias exists on Linux too) and assert that every consumer of the read
// record sees the same record whichever spelling it is handed.

// aliasedFile creates name under both spellings of one project and returns the
// two spellings of the file.
func aliasedFile(t *testing.T, name, content string) (realPath, aliasPath string) {
	t.Helper()
	realRoot, aliasRoot := aliasedProject(t)
	realPath = filepath.Join(realRoot, name)
	if err := os.WriteFile(realPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return realPath, filepath.Join(aliasRoot, name)
}

// peerRewrite replaces path's content the way an outside writer would, and moves
// its mtime clearly past anything recorded earlier.
func peerRewrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
}

func readVia(t *testing.T, tracker *ReadTracker, path string) {
	t.Helper()
	if _, err := NewReadFile(tracker).Execute(context.Background(), mustJSON(map[string]any{"file_path": path})); err != nil {
		t.Fatalf("read_file %s: %v", path, err)
	}
}

// TestWriteFile_StaleGuardHoldsAcrossSpellings is the issue's reproduction: read
// through one spelling, a peer changes the file, write through the other. The
// guard must refuse in both directions and leave the peer's content alone.
func TestWriteFile_StaleGuardHoldsAcrossSpellings(t *testing.T) {
	for _, tc := range []struct {
		name      string
		readAlias bool // read through the alias, write through the real path; else the reverse
	}{
		{name: "read alias, write real", readAlias: true},
		{name: "read real, write alias", readAlias: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			realPath, aliasPath := aliasedFile(t, "f.txt", "v1\n")
			readPath, writePath := realPath, aliasPath
			if tc.readAlias {
				readPath, writePath = aliasPath, realPath
			}
			tracker := NewReadTracker()
			readVia(t, tracker, readPath)
			peerRewrite(t, realPath, "peer change\n")

			_, err := NewWriteFile(WriteDeps{Reads: tracker, Writes: NewWriteTracker()}).Execute(context.Background(),
				mustJSON(map[string]any{"file_path": writePath, "content": "my overwrite\n"}))
			if err == nil || !strings.Contains(err.Error(), "changed on disk since you read it") {
				t.Fatalf("a write through another spelling of a file changed since the read must be refused, got: %v", err)
			}
			if got, _ := os.ReadFile(realPath); string(got) != "peer change\n" {
				t.Fatalf("the refused write clobbered the peer's change: %q", got)
			}
		})
	}
}

// TestWriteFile_OwnWritesAcrossSpellingsNotFlagged is the positive control: with
// no peer, a session alternating spellings is writing over its own knowledge and
// must never be refused. Before the fix the write through the alias compared the
// file against the stale alias-keyed READ, ignoring the session's own write
// recorded under the real path, and refused.
func TestWriteFile_OwnWritesAcrossSpellingsNotFlagged(t *testing.T) {
	realPath, aliasPath := aliasedFile(t, "f.txt", "v1\n")
	tracker := NewReadTracker()
	tool := NewWriteFile(WriteDeps{Reads: tracker, Writes: NewWriteTracker()})
	readVia(t, tracker, aliasPath)
	for i, p := range []string{realPath, aliasPath, realPath} {
		if _, err := tool.Execute(context.Background(), mustJSON(map[string]any{
			"file_path": p, "content": strings.Repeat("x", i+2) + "\n",
		})); err != nil {
			t.Fatalf("write %d via %s: the session's own writes must not be flagged: %v", i, p, err)
		}
	}
}

// TestEditFile_StrictModeAcceptsReadThroughAlias: strict mode's read-before-edit
// check must accept a read made through another spelling of the file. It failed
// closed before the fix ("has not been read"), the mirror image of the default
// guard failing open.
func TestEditFile_StrictModeAcceptsReadThroughAlias(t *testing.T) {
	realPath, aliasPath := aliasedFile(t, "f.go", "a\nb\n")
	tracker := NewReadTracker()
	deps := WriteDeps{Reads: tracker, Writes: NewWriteTracker(), Strict: func() bool { return true }}
	readVia(t, tracker, aliasPath)
	if _, err := NewEditFile(deps).Execute(context.Background(), mustJSON(map[string]any{
		"file_path": realPath,
		"edits":     []map[string]string{{"old_string": "a", "new_string": "A"}},
	})); err != nil {
		t.Fatalf("strict mode must accept a read made through another spelling: %v", err)
	}
	// Negative half: a peer change after the read is still refused, through
	// either spelling.
	peerRewrite(t, realPath, "A\nb\nPEER\n")
	_, err := NewEditFile(deps).Execute(context.Background(), mustJSON(map[string]any{
		"file_path": aliasPath,
		"edits":     []map[string]string{{"old_string": "b", "new_string": "B"}},
	}))
	if err == nil || !strings.Contains(err.Error(), "changed since you read it") {
		t.Fatalf("strict mode must refuse an edit over a peer change, got: %v", err)
	}
}

// TestWriteFile_StaleGuardHoldsAcrossCaseSpellings covers the case-variant alias
// on a volume that folds case (APFS, HFS+ and NTFS by default). It skips where
// the temp volume is case-sensitive, because the two spellings are then two
// files and there is nothing to merge.
func TestWriteFile_StaleGuardHoldsAcrossCaseSpellings(t *testing.T) {
	dir := t.TempDir()
	lower := filepath.Join(dir, "case.txt")
	if err := os.WriteFile(lower, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	upper := filepath.Join(dir, "CASE.txt")
	if a, err := os.Stat(lower); err != nil {
		t.Fatal(err)
	} else if b, err := os.Stat(upper); err != nil || !os.SameFile(a, b) {
		t.Skip("temp volume is case-sensitive; the case alias does not exist here")
	}
	tracker := NewReadTracker()
	readVia(t, tracker, upper)
	peerRewrite(t, lower, "peer change\n")
	_, err := NewWriteFile(WriteDeps{Reads: tracker}).Execute(context.Background(),
		mustJSON(map[string]any{"file_path": lower, "content": "my overwrite\n"}))
	if err == nil || !strings.Contains(err.Error(), "changed on disk since you read it") {
		t.Fatalf("a write through a case variant of a file changed since the read must be refused, got: %v", err)
	}
}

// TestReadTracker_PersistsCanonicalKey: the persisted read_tracking row carries
// the same key the in-memory map does, so a rehydrated read answers for every
// spelling exactly as the live one did.
func TestReadTracker_PersistsCanonicalKey(t *testing.T) {
	realPath, aliasPath := aliasedFile(t, "f.txt", "v1\n")
	rt := NewReadTracker()
	var persisted string
	rt.SetPersistSink(func(path string, _ time.Time, _ string) { persisted = path })
	rt.Record(aliasPath, time.Unix(1, 0), "sha")
	if want := lockPathKey(realPath); persisted != want {
		t.Fatalf("persisted key = %q, want the canonical key %q", persisted, want)
	}
	if recs := rt.Records(); len(recs) != 1 || recs[0].Path != lockPathKey(realPath) {
		t.Fatalf("Records() = %+v, want one record under %q", recs, lockPathKey(realPath))
	}
}

// TestReadTracker_HydrateCanonicalisesSpelledRows: rows persisted by a daemon
// that predates the fix carry the path as the agent spelled it. Hydration must
// fold them onto the canonical key, and when two spellings of one file collide
// the later read wins whichever order the store returns them in — it is the
// newest version the session is known to have seen.
func TestReadTracker_HydrateCanonicalisesSpelledRows(t *testing.T) {
	realPath, aliasPath := aliasedFile(t, "f.txt", "v1\n")
	older := ReadRecord{Path: aliasPath, Mtime: time.Unix(100, 0), SHA: "sha-old"}
	newer := ReadRecord{Path: realPath, Mtime: time.Unix(200, 0), SHA: "sha-new"}
	for name, recs := range map[string][]ReadRecord{
		"older first": {older, newer},
		"newer first": {newer, older},
	} {
		t.Run(name, func(t *testing.T) {
			rt := NewReadTracker()
			rt.Hydrate(recs)
			for _, p := range []string{realPath, aliasPath} {
				e, ok := rt.recorded(p)
				if !ok || e.sha != "sha-new" || !e.mtime.Equal(newer.Mtime) {
					t.Fatalf("recorded(%s) = (%+v, %v), want the newer read", p, e, ok)
				}
			}
			if n := len(rt.Records()); n != 1 {
				t.Fatalf("two spellings of one file hydrated into %d records, want 1", n)
			}
		})
	}
	// A spelled row alone must answer for the canonical spelling.
	rt := NewReadTracker()
	rt.Hydrate([]ReadRecord{older})
	if got := rt.Mtime(realPath); !got.Equal(older.Mtime) {
		t.Fatalf("Mtime(real) after hydrating the alias row = %v, want %v", got, older.Mtime)
	}
}
