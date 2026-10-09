//go:build darwin

package fswatch

// Unit tests of the darwin pump: FSEvents records go in through a channel the
// test controls, exactly as fsevents_darwin.go's callback hands them over.
// fsevents_darwin_test.go drives the real binding; fswatch_test.go and
// scenarios_test.go drive whole Watchers.

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/paths"
)

// testPump builds a Watcher and a pump the way startBackend wires them, with no
// stream behind them.
func testPump(t *testing.T, root, canon string, cooldown time.Duration, buffer int) (*Watcher, *fseventsPump) {
	t.Helper()
	w := testWatcher(root, buffer)
	return w, &fseventsPump{w: w, canon: canon, deb: newDebouncer(cooldown)}
}

func rec(path string, flags uint32) rawEvent { return rawEvent{path: path, flags: flags} }

func TestDebouncer(t *testing.T) {
	const cd = 100 * time.Millisecond
	t0 := time.Unix(1000, 0)
	var got []Event
	emit := func(ev Event) { got = append(got, ev) }

	d := newDebouncer(cd)
	d.add("/r/a", Create, t0)
	d.add("/r/a", Write, t0.Add(50*time.Millisecond))
	d.add("/r/b", Remove, t0.Add(60*time.Millisecond))
	d.flush(t0.Add(120*time.Millisecond), emit)
	if len(got) != 0 {
		t.Fatalf("flushed before either path was quiet for the cooldown: %v", got)
	}
	d.flush(t0.Add(150*time.Millisecond), emit)
	if !slices.Equal(got, []Event{{Path: "/r/a", Op: Create | Write}}) {
		t.Fatalf("after a's cooldown: got %v, want a with the union of its ops", got)
	}
	d.flush(t0.Add(160*time.Millisecond), emit)
	if len(got) != 2 || got[1] != (Event{Path: "/r/b", Op: Remove}) {
		t.Fatalf("after b's cooldown: got %v", got)
	}

	// A path rewritten faster than the cooldown is still delivered once it has
	// been held for maxHoldFactor cooldowns, rather than starving.
	got = nil
	d.add("/r/hot", Write, t0)
	for i := 1; i <= 4*maxHoldFactor; i++ {
		now := t0.Add(time.Duration(i) * cd / 2)
		d.add("/r/hot", Write, now)
		d.flush(now, emit)
	}
	if len(got) == 0 {
		t.Fatal("a path rewritten every half-cooldown was never delivered")
	}
}

func TestOpFromFlags(t *testing.T) {
	cases := []struct {
		flags uint32
		want  Op
	}{
		{fseItemCreated | fseItemIsFile, Create},
		{fseItemModified | fseItemIsFile, Write},
		{fseItemRemoved | fseItemIsDir, Remove},
		{fseItemRenamed | fseItemIsSymlink, Rename},
		{fseItemInodeMetaMod, Chmod},
		{fseItemFinderInfoMod, Chmod},
		{fseItemChangeOwner, Chmod},
		{fseItemXattrMod, Chmod},
		{fseItemCreated | fseItemModified | fseItemRemoved, Create | Write | Remove},
		{fseItemIsFile | fseItemIsDir | fseItemIsSymlink, 0}, // kinds alone are not changes
		{fseMustScanSubDirs, 0},
		{0, 0},
	}
	for _, tc := range cases {
		if got := opFromFlags(tc.flags); got != tc.want {
			t.Errorf("opFromFlags(%#x) = %05b, want %05b", tc.flags, got, tc.want)
		}
	}
}

func TestCutRoot(t *testing.T) {
	cases := []struct {
		path, root string
		fold       bool
		rest       string
		ok         bool
	}{
		{"/a/root", "/a/root", false, "", true},
		{"/a/root/x.go", "/a/root", false, "x.go", true},
		{"/a/root/d/x.go", "/a/root", false, "d/x.go", true},
		// Case-insensitive volume: the on-disk spelling is the same directory.
		{"/A/Root/x.go", "/a/root", true, "x.go", true},
		// Case-sensitive volume: /a/Root is ANOTHER directory and must never be
		// rewritten into the caller's root.
		{"/a/Root/x.go", "/a/root", false, "", false},
		{"/a/Root", "/a/root", false, "", false},
		{"/a/rootsibling/x.go", "/a/root", true, "", false},
		{"/a/roo", "/a/root", true, "", false},
		{"/b/root/x.go", "/a/root", true, "", false},
	}
	for _, tc := range cases {
		rest, ok := cutRoot(tc.path, tc.root, tc.fold)
		if rest != tc.rest || ok != tc.ok {
			t.Errorf("cutRoot(%q, %q, fold=%v) = %q, %v; want %q, %v", tc.path, tc.root, tc.fold, rest, ok, tc.rest, tc.ok)
		}
	}
}

// TestPump_LossFlagsSignalLost: every record that stands for changes FSEvents
// could not report one by one must ask for a reconcile; an ordinary record must
// not.
func TestPump_LossFlagsSignalLost(t *testing.T) {
	for _, f := range []uint32{fseMustScanSubDirs, fseUserDropped, fseKernelDropped, fseRootChanged, fseMount, fseUnmount, fseMustScanSubDirs | fseKernelDropped} {
		w, p := testPump(t, "/r", "/r", 0, 8)
		p.accept(rec("/r/sub", f), time.Now())
		if !lostSignalled(w) {
			t.Errorf("flags %#x did not signal Lost", f)
		}
	}
	w, p := testPump(t, "/r", "/r", 0, 8)
	p.accept(rec("/r/a.go", fseItemCreated|fseItemModified|fseItemIsFile), time.Now())
	if lostSignalled(w) {
		t.Error("an ordinary record signalled Lost")
	}
}

// TestPump_RootRecords: the root itself created, removed or renamed (replaced in
// place) means a reconcile; its own metadata changes mean nothing; neither is
// delivered as an event.
func TestPump_RootRecords(t *testing.T) {
	for _, f := range []uint32{fseItemCreated, fseItemRemoved, fseItemRenamed, fseItemRenamed | fseItemCreated} {
		w, p := testPump(t, "/v/root", "/p/v/root", 0, 8)
		p.accept(rec("/p/v/root", f|fseItemIsDir), time.Now())
		if !lostSignalled(w) {
			t.Errorf("root record %#x did not signal Lost", f)
		}
		if got := drain(w); len(got) != 0 {
			t.Errorf("root record %#x was delivered: %v", f, got)
		}
	}
	w, p := testPump(t, "/v/root", "/p/v/root", 0, 8)
	p.accept(rec("/p/v/root", fseItemInodeMetaMod|fseItemIsDir), time.Now())
	if lostSignalled(w) || len(drain(w)) != 0 {
		t.Error("a metadata change on the root signalled Lost or was delivered")
	}
}

// TestPump_RebasesOntoCallersRoot: FSEvents reports the resolved root. Created,
// removed and renamed-away paths must all come back under the caller's
// spelling; nothing is resolved further, so a symlink keeps its own name.
func TestPump_RebasesOntoCallersRoot(t *testing.T) {
	w, p := testPump(t, "/var/x/root", "/private/var/x/root", 0, 16)
	p.fold = true // the macOS default volume
	for _, ev := range []rawEvent{
		rec("/private/var/x/root/gone.go", fseItemRemoved|fseItemIsFile),
		rec("/private/var/x/root/old.go", fseItemRenamed|fseItemIsFile),
		rec("/private/var/x/root/sub/new.go", fseItemCreated|fseItemIsFile),
		rec("/private/var/x/root/alias.go", fseItemCreated|fseItemIsSymlink),
		rec("/PRIVATE/var/x/Root/case.go", fseItemModified|fseItemIsFile),
		// Shares a prefix, not a parent.
		rec("/private/var/x/rootsibling/a.go", fseItemModified|fseItemIsFile),
		// Excluded once rebased onto the caller's root.
		rec("/private/var/x/root/vendor/v.go", fseItemModified|fseItemIsFile),
	} {
		p.accept(ev, time.Now())
	}
	want := []Event{
		{Path: "/var/x/root/gone.go", Op: Remove},
		{Path: "/var/x/root/old.go", Op: Rename},
		{Path: "/var/x/root/sub/new.go", Op: Create},
		{Path: "/var/x/root/alias.go", Op: Create},
		{Path: "/var/x/root/case.go", Op: Write},
		{Path: "/private/var/x/rootsibling/a.go", Op: Write},
	}
	if got := drain(w); !slices.Equal(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
}

// TestPump_CaseSensitiveVolumeKeepsSiblingsApart: on a case-sensitive volume a
// directory differing from the root only by case is another directory; its
// paths must not be rewritten into the caller's root, where consumers would
// take them for the root's own files.
func TestPump_CaseSensitiveVolumeKeepsSiblingsApart(t *testing.T) {
	w, p := testPump(t, "/v/root", "/p/v/root", 0, 16)
	p.fold = false
	p.accept(rec("/p/v/Root/x.go", fseItemModified|fseItemIsFile), time.Now())
	p.accept(rec("/p/v/root/y.go", fseItemModified|fseItemIsFile), time.Now())
	want := []Event{
		{Path: "/p/v/Root/x.go", Op: Write}, // unchanged: outside the root
		{Path: "/v/root/y.go", Op: Write},
	}
	if got := drain(w); !slices.Equal(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
}

// TestPump_FoldMatchesTheVolume: the pump's case handling comes from the root's
// real volume, so on the case-insensitive default it folds, and the probe it
// relies on answers for that volume.
func TestPump_FoldMatchesTheVolume(t *testing.T) {
	// A lettered name: t.TempDir()'s own are digits, which have no case.
	root := filepath.Join(t.TempDir(), "casecheck")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	w, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	_, statErr := os.Lstat(filepath.Join(filepath.Dir(root), strings.ToUpper("casecheck")))
	volumeFolds := statErr == nil
	if got := paths.FoldsCase(paths.Canonical(root)); got != volumeFolds {
		t.Errorf("paths.FoldsCase = %v, but the upper-cased spelling exists = %v", got, volumeFolds)
	}
}

// TestPump_FullBufferSignalsLostWithoutBlocking: a consumer that stops reading
// must cost a reconcile, never back-pressure into the FSEvents callback.
func TestPump_FullBufferSignalsLostWithoutBlocking(t *testing.T) {
	w, p := testPump(t, "/r", "/r", 0, 1)
	batches := make(chan []rawEvent)
	go p.run(batches)
	t.Cleanup(func() { close(w.done) })
	for i := range 100 {
		select {
		case batches <- []rawEvent{rec(fmt.Sprintf("/r/f%d.go", i), fseItemModified)}:
		case <-time.After(5 * time.Second):
			t.Fatalf("the pump blocked on batch %d with a consumer that is not reading", i)
		}
	}
	select {
	case <-w.lost:
	case <-time.After(5 * time.Second):
		t.Fatal("overflowing the buffer did not signal Lost")
	}
	if n := len(drain(w)); n != 1 {
		t.Errorf("buffered %d events, want exactly the buffer's capacity of 1", n)
	}
}

// TestPump_DebouncesThroughRun: with a cooldown, repeated records for one path
// arrive as one event carrying the union of their ops.
func TestPump_DebouncesThroughRun(t *testing.T) {
	w, p := testPump(t, "/r", "/r", 40*time.Millisecond, 16)
	batches := make(chan []rawEvent)
	go p.run(batches)
	t.Cleanup(func() { close(w.done) })
	batches <- []rawEvent{rec("/r/a.go", fseItemCreated)}
	batches <- []rawEvent{rec("/r/a.go", fseItemModified), rec("/r/a.go", fseItemModified)}
	deadline := time.Now().Add(5 * time.Second)
	for len(w.events) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond) // anything else due would have arrived
	if got := drain(w); !slices.Equal(got, []Event{{Path: "/r/a.go", Op: Create | Write}}) {
		t.Errorf("got %v, want one event with the union of the ops", got)
	}
}

// TestPump_ExpandsDirectoryThatArrivedWhole: the pump hands a directory record
// to expandIfDir (expand_test.go covers the walk itself), after rebasing.
func TestPump_ExpandsDirectoryThatArrivedWhole(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(root, "moved")
	populate(t, moved, "pkg/b.go")
	w, p := testPump(t, root, root, 0, 64)
	p.accept(rec(moved, fseItemRenamed|fseItemIsDir), time.Now())
	if got := relPaths(root, drain(w)); !slices.Equal(got, []string{"moved", "moved/pkg", "moved/pkg/b.go"}) {
		t.Errorf("got %v", got)
	}
}

// TestPump_CloseWhileBusy closes the Watcher while events are held by the
// debouncer, the delivery buffer is full, and the stream is still handing over
// batches, the way Close and the binding's shutdown interleave in production.
func TestPump_CloseWhileBusy(t *testing.T) {
	w, p := testPump(t, "/r", "/r", time.Hour, 1) // nothing ever leaves the debouncer by time
	w.events <- Event{Path: "/r/full.go", Op: Write}
	batches := make(chan []rawEvent)
	srcDone := make(chan struct{})
	var feeders sync.WaitGroup
	for f := range 4 {
		feeders.Go(func() {
			for i := 0; ; i++ {
				b := []rawEvent{rec(fmt.Sprintf("/r/f%d-%d.go", f, i), fseItemModified)}
				if i%7 == 0 {
					b = append(b, rec("/r", fseMustScanSubDirs))
				}
				select {
				case batches <- b:
				case <-srcDone:
					return
				}
			}
		})
	}
	w.wg.Go(func() { p.run(batches) })
	// The binding's close releases a callback blocked on handing over a batch.
	w.stop = func() {
		close(srcDone)
		feeders.Wait()
	}
	time.Sleep(50 * time.Millisecond)

	finished := make(chan struct{})
	go func() {
		var closers sync.WaitGroup
		for range 3 {
			closers.Go(w.Close)
		}
		closers.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return while the pump was busy")
	}
	left := 0
	for range w.Events() {
		left++
	}
	t.Logf("%d buffered event(s) were still readable after Close", left)
	if _, ok := <-w.Lost(); ok {
		// One pending signal may remain buffered; after it the channel is closed.
		if _, ok := <-w.Lost(); ok {
			t.Error("Lost still open after Close")
		}
	}
}

// TestWatcher_ReportsContentsOfMovedInDirectory is the real-event companion of
// TestPump_ExpandsDirectoryThatArrivedWhole.
func TestWatcher_ReportsContentsOfMovedInDirectory(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, Options{Cooldown: 50 * time.Millisecond, ExcludeRegex: ExcludeDirsRegex(root)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := collect(t, w)
	warmUp(t, c, root)

	staging := filepath.Join(t.TempDir(), "pkg")
	populate(t, staging, "a.go", "sub/b.go", ".git/HEAD")
	if err := os.Rename(staging, filepath.Join(root, "pkg")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"pkg/a.go", "pkg/sub/b.go"} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if !waitFor(func() bool { return c.count(p, Create) > 0 }, 15*time.Second) {
			t.Errorf("no Create for %s inside a directory moved into the tree; got %v", rel, c.snapshot())
		}
	}
	if n := c.count(filepath.Join(root, "pkg", ".git", "HEAD"), 0); n != 0 {
		t.Errorf("an excluded file inside the moved directory was reported %d time(s)", n)
	}
}
