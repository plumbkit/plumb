package fswatch

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
)

// testWatcher builds a Watcher with no backend, for driving pumps and
// expansion directly. Its exclusion drops dot directories and vendor/.
func testWatcher(root string, buffer int) *Watcher {
	return &Watcher{
		root:        root,
		events:      make(chan Event, buffer),
		lost:        make(chan struct{}, 1),
		failed:      make(chan struct{}),
		exclude:     regexp.MustCompile(ExcludeDirsRegex(root, "vendor")),
		expandLimit: maxExpand,
		done:        make(chan struct{}),
	}
}

// drain returns everything buffered on w's Events without blocking.
func drain(w *Watcher) []Event {
	var out []Event
	for {
		select {
		case ev := <-w.events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

// relPaths returns the events' paths relative to root, slash-separated and
// sorted.
func relPaths(root string, evs []Event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		rel, _ := filepath.Rel(root, ev.Path)
		out = append(out, filepath.ToSlash(rel))
	}
	slices.Sort(out)
	return out
}

func lostSignalled(w *Watcher) bool {
	select {
	case <-w.lost:
		return true
	default:
		return false
	}
}

// populate creates each slash-separated file under dir.
func populate(t *testing.T, dir string, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestExpandIfDir_ReportsContents: a directory that arrives whole has every
// entry reported as created, minus excluded subtrees (pruned, not merely
// filtered), and a symlink inside it is reported without being followed.
func TestExpandIfDir_ReportsContents(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	populate(t, outside, "secret.go")
	moved := filepath.Join(root, "moved")
	populate(t, moved, "a.go", "pkg/b.go", "pkg/deep/c.go", "vendor/v.go", ".git/HEAD", "pkg/.cache/x")
	if err := os.Symlink(outside, filepath.Join(moved, "link")); err != nil {
		t.Fatal(err)
	}
	w := testWatcher(root, 64)
	w.expandIfDir(moved, Rename, func(p string) { w.deliver(Event{Path: p, Op: Create}) })
	want := []string{"moved/a.go", "moved/link", "moved/pkg", "moved/pkg/b.go", "moved/pkg/deep", "moved/pkg/deep/c.go"}
	if got := relPaths(root, drain(w)); !slices.Equal(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
	if lostSignalled(w) {
		t.Error("a complete expansion signalled Lost")
	}
}

// TestExpandIfDir_OnlyForNewDirectories: a write, a removal, or a created
// FILE is not expanded.
func TestExpandIfDir_OnlyForNewDirectories(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "dir")
	populate(t, dir, "x.go")
	w := testWatcher(root, 16)
	emit := func(p string) { w.deliver(Event{Path: p, Op: Create}) }
	w.expandIfDir(dir, Write|Chmod, emit)
	w.expandIfDir(dir, Remove, emit)
	w.expandIfDir(filepath.Join(dir, "x.go"), Create, emit)
	w.expandIfDir(filepath.Join(root, "gone"), Create, emit)
	if got := drain(w); len(got) != 0 {
		t.Errorf("expanded where it should not: %v", got)
	}
	w.expandIfDir(dir, Create, emit)
	if got := relPaths(root, drain(w)); !slices.Equal(got, []string{"dir/x.go"}) {
		t.Errorf("a created directory: got %v", got)
	}
}

// TestExpandIfDir_IncompleteSignalsLost: an expansion that cannot report
// everything, past its limit or through an unreadable subdirectory, asks for a
// reconcile.
func TestExpandIfDir_IncompleteSignalsLost(t *testing.T) {
	root := t.TempDir()
	moved := filepath.Join(root, "moved")
	populate(t, moved, "a.go", "b.go", "c.go")
	w := testWatcher(root, 64)
	w.expandLimit = 2
	w.expandIfDir(moved, Create, func(p string) { w.deliver(Event{Path: p, Op: Create}) })
	if !lostSignalled(w) {
		t.Error("an expansion past its limit did not signal Lost")
	}

	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 directory; the unreadable case cannot be staged")
	}
	locked := filepath.Join(root, "locked")
	populate(t, locked, "open/a.go", "shut/b.go")
	if err := os.Chmod(filepath.Join(locked, "shut"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(locked, "shut"), 0o755) })
	w = testWatcher(root, 64)
	w.expandIfDir(locked, Create, func(p string) { w.deliver(Event{Path: p, Op: Create}) })
	if !lostSignalled(w) {
		t.Error("an unreadable subdirectory did not signal Lost")
	}
	if got := relPaths(root, drain(w)); !slices.Contains(got, "locked/open/a.go") {
		t.Errorf("the readable sibling was not reported: %v", got)
	}
}
