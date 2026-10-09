package fswatch

// scenarios_test.go holds every backend to the package contract (fswatch.go)
// through whole Watchers and real filesystem changes. Each test runs on every
// platform with a backend; symlink and permission cases skip on Windows, where
// creating them needs privileges the test cannot assume.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func (c *collector) lostCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lost
}

// startWatcher starts a Watcher on root, collects what it reports, and waits
// until it is established.
func startWatcher(t *testing.T, root string, opts Options) *collector {
	t.Helper()
	w, err := New(root, opts)
	if err != nil {
		t.Fatalf("New(%s): %v", root, err)
	}
	c := collect(t, w)
	warmUp(t, c, root)
	return c
}

// expectEventually rewrites path until an event for it arrives: proof that the
// Watcher is reporting changes there.
func expectEventually(t *testing.T, c *collector, path, what string) {
	t.Helper()
	ok := waitFor(func() bool {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			return false // its directory may not exist yet
		}
		return waitFor(func() bool { return c.count(path, 0) > 0 }, 500*time.Millisecond)
	}, 30*time.Second)
	if !ok {
		t.Fatalf("%s: no event for %s", what, path)
	}
}

func skipOnWindows(t *testing.T, why string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip(why)
	}
}

func TestNew_RejectsMissingAndFileRoots(t *testing.T) {
	dir := t.TempDir()
	if _, err := New(filepath.Join(dir, "missing"), Options{}); err == nil {
		t.Error("New accepted a root that does not exist")
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(file, Options{}); err == nil {
		t.Error("New accepted a root that is a file")
	}
}

// TestWatcher_SymlinkReportedUnderOwnName: creating, renaming and removing an
// in-tree symlink are reported under the link's own name. The topology index
// keeps such a link under its own name, so an event naming the target instead
// leaves it unindexed.
func TestWatcher_SymlinkReportedUnderOwnName(t *testing.T) {
	skipOnWindows(t, "creating symlinks needs privileges on Windows")
	root := t.TempDir()
	target := filepath.Join(root, "real.go")
	if err := os.WriteFile(target, []byte("package r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := startWatcher(t, root, Options{Cooldown: 30 * time.Millisecond})
	alias, alias2 := filepath.Join(root, "alias.go"), filepath.Join(root, "alias2.go")

	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	if !waitFor(func() bool { return c.count(alias, 0) > 0 }, 15*time.Second) {
		t.Fatalf("creating a symlink was not reported under its own name; got %v", c.snapshot())
	}
	if err := os.Rename(alias, alias2); err != nil {
		t.Fatal(err)
	}
	if !waitFor(func() bool { return c.count(alias2, 0) > 0 }, 15*time.Second) {
		t.Errorf("renaming a symlink was not reported under its new name; got %v", c.snapshot())
	}
	if err := os.Remove(alias2); err != nil {
		t.Fatal(err)
	}
	if !waitFor(func() bool { return c.count(alias2, Remove|Rename) > 0 }, 15*time.Second) {
		t.Errorf("removing a symlink was not reported under its own name; got %v", c.snapshot())
	}
}

// writeGhosts keeps writing ghost.go into a directory that has left the root,
// for a while. A watch still attached to that directory (inotify follows the
// inode) would report the writes under the ROOT's path: an event from an old
// generation escaping into the new one.
func writeGhosts(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(600 * time.Millisecond)
	for i := 0; time.Now().Before(deadline); i++ {
		if err := os.WriteFile(filepath.Join(dir, "ghost.go"), []byte(strconv.Itoa(i)), 0o644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// assertNoGhosts fails if any event names ghost.go under the root, or any path
// outside it.
func assertNoGhosts(t *testing.T, c *collector, root string) {
	t.Helper()
	for _, ev := range c.snapshot() {
		if filepath.Base(ev.Path) == "ghost.go" {
			t.Errorf("an old generation's change escaped: %v", ev)
		}
		if ev.Path != root && !strings.HasPrefix(ev.Path, root+string(filepath.Separator)) {
			t.Errorf("an event names a path outside the root: %v", ev)
		}
	}
}

// TestWatcher_RootRenamedAndRecreated: a root directory renamed away asks for
// a reconcile, changes in the directory that left are never reported, and once
// a directory exists at the root path again, changes in it are. Before, the
// watcher went silently dead on every platform.
func TestWatcher_RootRenamedAndRecreated(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	c := startWatcher(t, root, Options{Cooldown: 30 * time.Millisecond})
	lostBefore := c.lostCount()

	away := filepath.Join(base, "ws.away")
	if err := os.Rename(root, away); err != nil {
		t.Fatal(err)
	}
	writeGhosts(t, away)
	if !waitFor(func() bool { return c.lostCount() > lostBefore }, 15*time.Second) {
		t.Error("renaming the root away did not signal Lost")
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	expectEventually(t, c, filepath.Join(root, "after.go"), "root recreated")
	writeGhosts(t, away)
	time.Sleep(200 * time.Millisecond)
	assertNoGhosts(t, c, root)
}

// TestWatcher_RootReplacedInPlace: another, populated, directory moved into the
// root path asks for a reconcile (its contents were never reported), later
// changes inside it are reported, and the replaced directory is never heard
// from again. Three times over, so each generation hands over to the next.
func TestWatcher_RootReplacedInPlace(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	c := startWatcher(t, root, Options{Cooldown: 30 * time.Millisecond})
	for gen := range 3 {
		staging := filepath.Join(base, fmt.Sprintf("ws.new%d", gen))
		populate(t, staging, "pkg/existing.go")
		old := filepath.Join(base, fmt.Sprintf("ws.old%d", gen))
		lostBefore := c.lostCount()

		if err := os.Rename(root, old); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(staging, root); err != nil {
			t.Fatal(err)
		}
		if !waitFor(func() bool { return c.lostCount() > lostBefore }, 15*time.Second) {
			t.Errorf("generation %d: replacing the root did not signal Lost", gen)
		}
		expectEventually(t, c, filepath.Join(root, "pkg", fmt.Sprintf("later%d.go", gen)), fmt.Sprintf("generation %d", gen))
		writeGhosts(t, old)
	}
	time.Sleep(200 * time.Millisecond)
	assertNoGhosts(t, c, root)
}

// TestWatcher_CloseWhileRootIsMissing: Close returns promptly while the root is
// gone, which on inotify is while the supervisor is waiting and backing off.
func TestWatcher_CloseWhileRootIsMissing(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	w, err := New(root, Options{Cooldown: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	c := collect(t, w)
	warmUp(t, c, root)
	lostBefore := c.lostCount()
	if err := os.Rename(root, filepath.Join(base, "ws.away")); err != nil {
		t.Fatal(err)
	}
	if !waitFor(func() bool { return c.lostCount() > lostBefore }, 15*time.Second) {
		t.Error("renaming the root away did not signal Lost")
	}
	time.Sleep(2 * time.Second) // into the backoff
	closed := make(chan struct{})
	go func() { w.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return while the root was missing")
	}
}

// TestWatcher_SymlinkedRootKeepsCallersSpelling: a root given through a
// symlink is reported under that spelling, which is what callers relate paths
// to.
func TestWatcher_SymlinkedRootKeepsCallersSpelling(t *testing.T) {
	skipOnWindows(t, "creating symlinks needs privileges on Windows")
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	c := startWatcher(t, link, Options{Cooldown: 30 * time.Millisecond})
	if err := os.WriteFile(filepath.Join(realDir, "via-real.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(link, "via-real.go")
	if !waitFor(func() bool { return c.count(want, 0) > 0 }, 15*time.Second) {
		t.Errorf("no event under the caller's spelling %s; got %v", want, c.snapshot())
	}
}

// TestWatcher_NamesAndDepth: spaces, non-ASCII names and a deep path all come
// back as paths naming the changed file.
func TestWatcher_NamesAndDepth(t *testing.T) {
	root := t.TempDir()
	c := startWatcher(t, root, Options{Cooldown: 30 * time.Millisecond})
	deep := root
	for i := range 20 {
		deep = filepath.Join(deep, fmt.Sprintf("level-%02d", i))
	}
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	names := []string{
		filepath.Join(root, "with space.go"),
		filepath.Join(root, "naïve café.go"),
		filepath.Join(root, "日本語.go"),
		filepath.Join(deep, "deep.go"),
	}
	for _, p := range names {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range names {
		wantInfo, err := os.Stat(want)
		if err != nil {
			t.Fatal(err)
		}
		found := waitFor(func() bool {
			for _, ev := range c.snapshot() {
				if fi, err := os.Stat(ev.Path); err == nil && os.SameFile(fi, wantInfo) {
					return true
				}
			}
			return false
		}, 15*time.Second)
		if !found {
			t.Errorf("no event names %q", strings.TrimPrefix(want, root))
		}
	}
}

// TestWatcher_BurstIsReportedOrLost: thousands of files written as fast as
// possible are each reported, or the Watcher says it lost some. Never silence.
func TestWatcher_BurstIsReportedOrLost(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "burst")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	c := startWatcher(t, root, Options{Cooldown: 30 * time.Millisecond})
	lostBefore := c.lostCount()
	const files = 2000
	for i := range files {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%04d.go", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reported := func() int {
		seen := map[string]bool{}
		for _, ev := range c.snapshot() {
			if filepath.Dir(ev.Path) == dir {
				seen[ev.Path] = true
			}
		}
		return len(seen)
	}
	if !waitFor(func() bool { return reported() == files || c.lostCount() > lostBefore }, 60*time.Second) {
		t.Fatalf("%d of %d files reported and no Lost: changes vanished silently", reported(), files)
	}
	t.Logf("%d of %d reported; Lost signalled: %v", reported(), files, c.lostCount() > lostBefore)
}

// TestWatcher_NestedAndSiblingRoots: plumb watches nested workspaces (a
// checkout inside another) with separate Watchers. Both see a change in the
// inner one; a sibling sees nothing of it.
func TestWatcher_NestedAndSiblingRoots(t *testing.T) {
	base := t.TempDir()
	outer := filepath.Join(base, "outer")
	inner := filepath.Join(outer, "inner")
	sibling := filepath.Join(base, "sibling")
	for _, d := range []string{inner, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	opts := Options{Cooldown: 30 * time.Millisecond}
	co := startWatcher(t, outer, opts)
	ci := startWatcher(t, inner, opts)
	cs := startWatcher(t, sibling, opts)
	target := filepath.Join(inner, "x.go")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]*collector{"outer": co, "inner": ci} {
		if !waitFor(func() bool { return c.count(target, 0) > 0 }, 15*time.Second) {
			t.Errorf("the %s watcher did not report %s", name, target)
		}
	}
	time.Sleep(300 * time.Millisecond)
	for _, ev := range cs.snapshot() {
		if !strings.HasPrefix(ev.Path, sibling+string(filepath.Separator)) {
			t.Errorf("the sibling watcher reported %s", ev.Path)
		}
	}
}

// TestWatcher_DirectoryRenamedWithinTree: renaming a populated directory
// reports its contents under the new name and the old name as gone.
func TestWatcher_DirectoryRenamedWithinTree(t *testing.T) {
	root := t.TempDir()
	populate(t, filepath.Join(root, "a"), "x.go", "y/z.go")
	c := startWatcher(t, root, Options{Cooldown: 30 * time.Millisecond})
	if err := os.Rename(filepath.Join(root, "a"), filepath.Join(root, "b")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"b/x.go", "b/y/z.go"} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if !waitFor(func() bool { return c.count(p, 0) > 0 }, 15*time.Second) {
			t.Errorf("%s inside the renamed directory was not reported; got %v", rel, c.snapshot())
		}
	}
	if !waitFor(func() bool { return c.count(filepath.Join(root, "a"), Rename|Remove) > 0 }, 15*time.Second) {
		t.Errorf("the old directory name was not reported gone; got %v", c.snapshot())
	}
}

// TestWatcher_TreeRemoved: deleting a directory tree is reported.
func TestWatcher_TreeRemoved(t *testing.T) {
	root := t.TempDir()
	gone := filepath.Join(root, "gone")
	populate(t, gone, "a.go", "b/c.go")
	c := startWatcher(t, root, Options{Cooldown: 30 * time.Millisecond})
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	reported := func() bool {
		for _, ev := range c.snapshot() {
			if (ev.Path == gone || strings.HasPrefix(ev.Path, gone+string(filepath.Separator))) && ev.Op.Has(Remove|Rename) {
				return true
			}
		}
		return false
	}
	if !waitFor(reported, 15*time.Second) {
		t.Errorf("removing a tree reported nothing removed; got %v", c.snapshot())
	}
}

func TestWatcher_HardLinkAndChmod(t *testing.T) {
	skipOnWindows(t, "hard links and mode bits behave differently on Windows")
	root := t.TempDir()
	orig := filepath.Join(root, "orig.go")
	if err := os.WriteFile(orig, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := startWatcher(t, root, Options{Cooldown: 30 * time.Millisecond})
	hard := filepath.Join(root, "hard.go")
	if err := os.Link(orig, hard); err != nil {
		t.Fatal(err)
	}
	if !waitFor(func() bool { return c.count(hard, 0) > 0 }, 15*time.Second) {
		t.Errorf("a hard link was not reported; got %v", c.snapshot())
	}
	before := c.count(orig, 0)
	if err := os.Chmod(orig, 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitFor(func() bool { return c.count(orig, 0) > before }, 15*time.Second) {
		t.Errorf("a permission change was not reported; got %v", c.snapshot())
	}
}

// TestWatcher_CyclesDoNotLeak: Watchers started and closed repeatedly, each
// seeing traffic, leave no goroutines behind.
func TestWatcher_CyclesDoNotLeak(t *testing.T) {
	root := t.TempDir()
	baseline := runtime.NumGoroutine()
	for i := range 30 {
		w, err := New(root, Options{Cooldown: 10 * time.Millisecond})
		if err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
		_ = os.WriteFile(filepath.Join(root, fmt.Sprintf("c%d.go", i)), []byte("x"), 0o644)
		time.Sleep(5 * time.Millisecond)
		w.Close()
	}
	if !waitFor(func() bool { return runtime.NumGoroutine() <= baseline+2 }, 10*time.Second) {
		t.Errorf("goroutines grew from %d to %d over 30 start/stop cycles", baseline, runtime.NumGoroutine())
	}
}
