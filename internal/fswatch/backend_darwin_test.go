//go:build darwin

package fswatch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/fswatcher/fswatcher"
)

// testPump builds a Watcher and a pump around channels the test controls, the
// way startBackend wires them to the library.
func testPump(t *testing.T, root, canon string, cooldown time.Duration, buffer int) (*Watcher, *fseventsPump) {
	t.Helper()
	w := testWatcher(root, buffer)
	return w, &fseventsPump{w: w, canon: canon, deb: newDebouncer(cooldown)}
}

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
	for i := 1; i <= 2*maxHoldFactor*2; i++ {
		now := t0.Add(time.Duration(i) * cd / 2)
		d.add("/r/hot", Write, now)
		d.flush(now, emit)
	}
	if len(got) == 0 {
		t.Fatal("a path rewritten every half-cooldown was never delivered")
	}
}

func TestPump_AnyErrorSignalsLost(t *testing.T) {
	w, p := testPump(t, "/r", "/r", 0, 8)
	evs := make(chan fswatcher.Event)
	errs := make(chan error)
	done := make(chan struct{})
	go func() { p.run(evs, errs); close(done) }()
	errs <- errors.New("an error whose text no one matches")
	select {
	case <-w.lost:
	case <-time.After(5 * time.Second):
		t.Fatal("an error from the library did not signal Lost")
	}
	close(evs)
	close(errs)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the pump did not return when the library closed its channels")
	}
}

// TestPump_FullBufferSignalsLostWithoutBlocking: a consumer that stops reading
// must cost a reconcile, never back-pressure into the library (which would
// stall its FSEvents dispatch queue).
func TestPump_FullBufferSignalsLostWithoutBlocking(t *testing.T) {
	w, p := testPump(t, "/r", "/r", 0, 1)
	evs := make(chan fswatcher.Event)
	errs := make(chan error)
	go p.run(evs, errs)
	t.Cleanup(func() { close(evs); close(errs) })
	for i := range 100 {
		select {
		case evs <- fswatcher.Event{Name: fmt.Sprintf("/r/f%d.go", i), Op: fswatcher.Write}:
		case <-time.After(5 * time.Second):
			t.Fatalf("the pump blocked on event %d with a consumer that is not reading", i)
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

// TestPump_RebasesOntoCallersRoot: FSEvents reports the resolved root, and the
// library resolves symlinks in every path it can. Created, removed and
// renamed-away paths must all come back under the caller's spelling.
func TestPump_RebasesOntoCallersRoot(t *testing.T) {
	w, p := testPump(t, "/var/x/root", "/private/var/x/root", 0, 16)
	for _, ev := range []fswatcher.Event{
		{Name: "/private/var/x/root/gone.go", Op: fswatcher.Remove},
		{Name: "/private/var/x/root/old.go", Op: fswatcher.Rename},
		{Name: "/private/var/x/root/sub/new.go", Op: fswatcher.Create},
		// Shares a prefix, not a parent.
		{Name: "/private/var/x/rootsibling/a.go", Op: fswatcher.Write},
		// A symlink the library resolved out of the tree.
		{Name: "/elsewhere/a.go", Op: fswatcher.Write},
		// Excluded once rebased onto the caller's root.
		{Name: "/private/var/x/root/vendor/v.go", Op: fswatcher.Write},
	} {
		p.accept(ev, time.Now())
	}
	want := []Event{
		{Path: "/var/x/root/gone.go", Op: Remove},
		{Path: "/var/x/root/old.go", Op: Rename},
		{Path: "/var/x/root/sub/new.go", Op: Create},
		{Path: "/private/var/x/rootsibling/a.go", Op: Write},
		{Path: "/elsewhere/a.go", Op: Write},
	}
	if got := drain(w); !slices.Equal(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
	if got := p.rebase("/private/var/x/root"); got != "/var/x/root" {
		t.Errorf("rebase(root) = %q", got)
	}
}

// TestPump_ExpandsDirectoryThatArrivedWhole: the pump hands a directory event
// to expandIfDir (expand_test.go covers the walk itself), after rebasing.
func TestPump_ExpandsDirectoryThatArrivedWhole(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(root, "moved")
	if err := os.MkdirAll(filepath.Join(moved, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moved, "pkg", "b.go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	w, p := testPump(t, root, root, 0, 64)
	p.accept(fswatcher.Event{Name: moved, Op: fswatcher.Rename}, time.Now())
	if got := relPaths(root, drain(w)); !slices.Equal(got, []string{"moved", "moved/pkg", "moved/pkg/b.go"}) {
		t.Errorf("got %v", got)
	}
}

// TestPump_CloseWhileBusy closes the Watcher while events are held by the
// debouncer, the delivery buffer is full, and the library is still sending, the
// way Close and the library's own shutdown interleave in production.
func TestPump_CloseWhileBusy(t *testing.T) {
	w, p := testPump(t, "/r", "/r", time.Hour, 1) // nothing ever leaves the debouncer by time
	w.events <- Event{Path: "/r/full.go", Op: Write}
	evs := make(chan fswatcher.Event)
	errs := make(chan error)
	srcDone := make(chan struct{})
	var feeders sync.WaitGroup
	for f := range 4 {
		feeders.Go(func() {
			for i := 0; ; i++ {
				select {
				case evs <- fswatcher.Event{Name: fmt.Sprintf("/r/f%d-%d.go", f, i), Op: fswatcher.Write}:
				case errs <- errors.New("rescan"):
				case <-srcDone:
					return
				}
			}
		})
	}
	w.wg.Go(func() { p.run(evs, errs) })
	// The library's Close stops its senders and then closes its channels.
	w.stop = func() {
		close(srcDone)
		feeders.Wait()
		close(evs)
		close(errs)
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
	for _, rel := range []string{"a.go", "sub/b.go", ".git/HEAD"} {
		p := filepath.Join(staging, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("package pkg\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
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
