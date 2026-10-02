package tools

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/plumbkit/plumb/internal/history"
)

// find_replace scans every file before it takes that file's lock. A write that
// lands in the gap must survive: the replacement is applied to the bytes on
// disk under the lock, and history records those bytes as Before. The dirty
// guard runs under the lock, so its hook stands in for the interleaved writer.
func TestFindReplaceKeepsAWriteThatLandsAfterTheScan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	scanned := "alpha one\nbeta\n"
	landed := "alpha one\nbeta\ngamma from a peer\n"
	if err := os.WriteFile(path, []byte(scanned), 0o644); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	var f fakeHistory
	deps := WriteDeps{
		HistoryFn:   f.record,
		WorkspaceFn: func(context.Context) string { return dir },
		BlockDirtyFn: func() bool {
			once.Do(func() {
				if err := os.WriteFile(path, []byte(landed), 0o644); err != nil {
					t.Error(err)
				}
			})
			return false
		},
	}
	_, err := NewFindReplace(deps).Execute(context.Background(), mustJSON(map[string]any{
		"path": dir, "pattern": "alpha", "replacement": "ALPHA", "dry_run": false,
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := "ALPHA one\nbeta\ngamma from a peer\n"
	if got, _ := os.ReadFile(path); string(got) != want {
		t.Fatalf("the interleaved write was lost:\n got %q\nwant %q", got, want)
	}
	c := f.only(t)
	if c.Op != history.OpUpdate || string(c.Before.Content) != landed {
		t.Fatalf("history Before is not what the write replaced: %q", c.Before.Content)
	}
	assertMatchesDisk(t, c)
}

// The positive control's mirror: when the interleaved write removes every
// match, nothing is written and nothing is recorded.
func TestFindReplaceSkipsAFileThatNoLongerMatchesUnderTheLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	landed := "no match left\n"
	var once sync.Once
	var f fakeHistory
	deps := WriteDeps{
		HistoryFn:   f.record,
		WorkspaceFn: func(context.Context) string { return dir },
		BlockDirtyFn: func() bool {
			once.Do(func() {
				if err := os.WriteFile(path, []byte(landed), 0o644); err != nil {
					t.Error(err)
				}
			})
			return false
		},
	}
	_, err := NewFindReplace(deps).Execute(context.Background(), mustJSON(map[string]any{
		"path": dir, "pattern": "alpha", "replacement": "ALPHA", "dry_run": false,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != landed {
		t.Fatalf("a stale replacement overwrote the file: %q", got)
	}
	if cs := f.all(); len(cs) != 0 {
		t.Fatalf("recorded %d changes for a file that no longer matched: %+v", len(cs), cs)
	}
}
