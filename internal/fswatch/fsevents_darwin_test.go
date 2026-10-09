//go:build darwin

package fswatch

// Tests of the FSEvents binding itself, against the target CoreServices API: the
// flags and paths it reports, that streams stay isolated from each other, and
// that closing one retires every native resource and callback. Run them with
// CGO_ENABLED=0 as well as with cgo, on both architectures where possible: the
// binding is purego either way, but the runtime underneath it differs.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// rawLog collects what one stream reports.
type rawLog struct {
	mu  sync.Mutex
	evs []rawEvent
}

func (l *rawLog) snapshot() []rawEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]rawEvent(nil), l.evs...)
}

// flagsFor ORs together every flag reported for path.
func (l *rawLog) flagsFor(path string) uint32 {
	var f uint32
	for _, ev := range l.snapshot() {
		if ev.path == path {
			f |= ev.flags
		}
	}
	return f
}

func (l *rawLog) sawFlag(flag uint32) bool {
	for _, ev := range l.snapshot() {
		if ev.flags&flag != 0 {
			return true
		}
	}
	return false
}

// openRaw opens a stream on root (resolved, so paths compare as FSEvents spells
// them) and records everything it reports until the test ends.
func openRaw(t *testing.T, root string) (*fseStream, *rawLog) {
	t.Helper()
	s, err := openFSEventStream(root)
	if err != nil {
		t.Fatalf("openFSEventStream: %v", err)
	}
	l := &rawLog{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case b := <-s.batches:
				l.mu.Lock()
				l.evs = append(l.evs, b...)
				l.mu.Unlock()
			case <-s.closed:
				return
			}
		}
	}()
	t.Cleanup(func() { s.close(); <-done })
	return s, l
}

// resolvedTempDir is t.TempDir() with symlinks resolved, the spelling FSEvents
// reports.
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// rawWarmUp writes a marker until the stream reports it: records for changes
// made before a stream is established are never delivered.
func rawWarmUp(t *testing.T, l *rawLog, root string) {
	t.Helper()
	p := filepath.Join(root, ".warm")
	ok := waitFor(func() bool {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return waitFor(func() bool { return l.flagsFor(p) != 0 }, 500*time.Millisecond)
	}, 30*time.Second)
	if !ok {
		t.Fatal("the stream reported nothing at all")
	}
}

func expectFlags(t *testing.T, l *rawLog, path string, want uint32, what string) {
	t.Helper()
	if !waitFor(func() bool { return l.flagsFor(path)&want == want }, 10*time.Second) {
		t.Errorf("%s: %s has flags %#x, want all of %#x", what, filepath.Base(path), l.flagsFor(path), want)
	}
}

func TestFSEventStream_FileLifecycle(t *testing.T) {
	root := resolvedTempDir(t)
	_, l := openRaw(t, root)
	rawWarmUp(t, l, root)
	a, b := filepath.Join(root, "a.go"), filepath.Join(root, "b.go")

	if err := os.WriteFile(a, []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expectFlags(t, l, a, fseItemCreated|fseItemIsFile, "create")
	if err := os.WriteFile(a, []byte("package a\n// more\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expectFlags(t, l, a, fseItemModified, "write")
	if err := os.Chmod(a, 0o600); err != nil {
		t.Fatal(err)
	}
	expectFlags(t, l, a, fseItemInodeMetaMod, "chmod")
	if err := os.Rename(a, b); err != nil {
		t.Fatal(err)
	}
	expectFlags(t, l, a, fseItemRenamed, "rename, old name")
	expectFlags(t, l, b, fseItemRenamed|fseItemIsFile, "rename, new name")
	if err := os.Remove(b); err != nil {
		t.Fatal(err)
	}
	expectFlags(t, l, b, fseItemRemoved, "remove")
}

func TestFSEventStream_Directories(t *testing.T) {
	root := resolvedTempDir(t)
	_, l := openRaw(t, root)
	rawWarmUp(t, l, root)
	d := filepath.Join(root, "dir")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	expectFlags(t, l, d, fseItemCreated|fseItemIsDir, "mkdir")
	nested := filepath.Join(d, "x", "y")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	expectFlags(t, l, nested, fseItemCreated|fseItemIsDir, "mkdir -p, deepest")
	if err := os.RemoveAll(d); err != nil {
		t.Fatal(err)
	}
	expectFlags(t, l, d, fseItemRemoved|fseItemIsDir, "rm -r")
}

// TestFSEventStream_SymlinkKeepsItsOwnName is the reason this binding exists:
// a symlink's creation, rename and removal are reported under the link's own
// name, while a write through it is reported under the file actually changed.
func TestFSEventStream_SymlinkKeepsItsOwnName(t *testing.T) {
	root := resolvedTempDir(t)
	target := filepath.Join(root, "real.go")
	if err := os.WriteFile(target, []byte("package r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, l := openRaw(t, root)
	rawWarmUp(t, l, root)
	alias, alias2 := filepath.Join(root, "alias.go"), filepath.Join(root, "alias2.go")

	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	expectFlags(t, l, alias, fseItemCreated|fseItemIsSymlink, "symlink created")
	if err := os.Rename(alias, alias2); err != nil {
		t.Fatal(err)
	}
	expectFlags(t, l, alias2, fseItemRenamed|fseItemIsSymlink, "symlink renamed, new name")
	if err := os.WriteFile(alias2, []byte("package r\n// via the link\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expectFlags(t, l, target, fseItemModified, "write through the link")
	if err := os.Remove(alias2); err != nil {
		t.Fatal(err)
	}
	expectFlags(t, l, alias2, fseItemRemoved|fseItemIsSymlink, "symlink removed")
	if l.flagsFor(target)&(fseItemRemoved|fseItemRenamed) != 0 {
		t.Errorf("the target was reported removed or renamed: %#x", l.flagsFor(target))
	}
}

// TestFSEventStream_RootRenamedAndRecreated: an FSEvents stream watches the
// PATH, and the binding never drops one. A root renamed away and recreated, and
// then a root replaced in place by a populated directory, keep being reported.
func TestFSEventStream_RootRenamedAndRecreated(t *testing.T) {
	base := resolvedTempDir(t)
	root := filepath.Join(base, "ws")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	_, l := openRaw(t, root)
	rawWarmUp(t, l, root)

	if err := os.Rename(root, filepath.Join(base, "ws.away")); err != nil {
		t.Fatal(err)
	}
	expectFlags(t, l, root, fseItemRenamed|fseItemIsDir, "root renamed away")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	after := filepath.Join(root, "after.go")
	ok := waitFor(func() bool {
		_ = os.WriteFile(after, []byte("x"), 0o644)
		return waitFor(func() bool { return l.flagsFor(after) != 0 }, 500*time.Millisecond)
	}, 15*time.Second)
	if !ok {
		t.Fatal("nothing reported in a root recreated at the watched path")
	}

	staging := filepath.Join(base, "ws.new")
	populate(t, staging, "pkg/new.go")
	if err := os.Rename(root, filepath.Join(base, "ws.old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staging, root); err != nil {
		t.Fatal(err)
	}
	later := filepath.Join(root, "pkg", "later.go")
	ok = waitFor(func() bool {
		_ = os.WriteFile(later, []byte("x"), 0o644)
		return waitFor(func() bool { return l.flagsFor(later) != 0 }, 500*time.Millisecond)
	}, 15*time.Second)
	if !ok {
		t.Fatal("nothing reported in a root replaced in place by a populated directory")
	}
}

// TestFSEventStream_StreamsAreIsolated: every stream shares one callback, routed
// by id. Concurrent streams over sibling directories must each see only their
// own changes.
func TestFSEventStream_StreamsAreIsolated(t *testing.T) {
	base := resolvedTempDir(t)
	const n = 8
	roots := make([]string, n)
	logs := make([]*rawLog, n)
	for i := range n {
		roots[i] = filepath.Join(base, fmt.Sprintf("r%d", i))
		if err := os.Mkdir(roots[i], 0o755); err != nil {
			t.Fatal(err)
		}
		_, logs[i] = openRaw(t, roots[i])
	}
	for i := range n {
		rawWarmUp(t, logs[i], roots[i])
	}
	for i := range n {
		for j := range 5 {
			if err := os.WriteFile(filepath.Join(roots[i], fmt.Sprintf("f%d.go", j)), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := range n {
		mine := filepath.Join(roots[i], "f4.go")
		if !waitFor(func() bool { return logs[i].flagsFor(mine) != 0 }, 10*time.Second) {
			t.Errorf("stream %d never reported its own file", i)
		}
		for _, ev := range logs[i].snapshot() {
			if !strings.HasPrefix(ev.path, roots[i]+string(filepath.Separator)) && ev.path != roots[i] {
				t.Errorf("stream %d reported %s, outside its root", i, ev.path)
			}
		}
	}
}

// TestFSEventStream_BurstIsReportedOrRescanned: a burst far larger than the
// stream's batch buffer must not lose anything silently. Every file is
// reported, or FSEvents says it dropped records.
func TestFSEventStream_BurstIsReportedOrRescanned(t *testing.T) {
	root := resolvedTempDir(t)
	_, l := openRaw(t, root)
	rawWarmUp(t, l, root)
	const files = 3000
	dir := filepath.Join(root, "burst")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range files {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%04d.go", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reported := func() int {
		seen := map[string]bool{}
		for _, ev := range l.snapshot() {
			if filepath.Dir(ev.path) == dir {
				seen[ev.path] = true
			}
		}
		return len(seen)
	}
	rescan := func() bool { return l.sawFlag(fseMustScanSubDirs | fseUserDropped | fseKernelDropped) }
	if !waitFor(func() bool { return reported() == files || rescan() }, 30*time.Second) {
		t.Fatalf("%d of %d files reported and no rescan flag: records were lost silently", reported(), files)
	}
	t.Logf("%d of %d files reported; rescan flag seen: %v", reported(), files, rescan())
}

// TestFSEventStream_PathsAreExact: names with spaces and non-ASCII characters,
// and a deep path, come back as paths that name the file.
func TestFSEventStream_PathsAreExact(t *testing.T) {
	root := resolvedTempDir(t)
	_, l := openRaw(t, root)
	rawWarmUp(t, l, root)
	deep := root
	for i := range 30 {
		deep = filepath.Join(deep, fmt.Sprintf("level-%02d-xxxxxxxxxx", i))
	}
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	names := []string{
		filepath.Join(root, "with space.go"),
		filepath.Join(root, "naïve café.go"),
		filepath.Join(root, "日本語.go"),
		filepath.Join(root, "emoji 😀.go"),
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
			for _, ev := range l.snapshot() {
				if fi, err := os.Stat(ev.path); err == nil && os.SameFile(fi, wantInfo) {
					return true
				}
			}
			return false
		}, 10*time.Second)
		if !found {
			t.Errorf("no reported path names %q", filepath.Base(want))
		}
	}
}

func TestFSEventStream_HardLink(t *testing.T) {
	root := resolvedTempDir(t)
	orig := filepath.Join(root, "orig.go")
	if err := os.WriteFile(orig, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, l := openRaw(t, root)
	rawWarmUp(t, l, root)
	hard := filepath.Join(root, "hard.go")
	if err := os.Link(orig, hard); err != nil {
		t.Fatal(err)
	}
	expectFlags(t, l, hard, fseItemCreated, "hard link")
}

// TestFSEventStream_CloseRetiresEverything opens and closes many streams under
// event traffic, closing each from several goroutines at once, and checks that
// the registry empties, nothing is delivered after close, and no goroutines
// pile up.
func TestFSEventStream_CloseRetiresEverything(t *testing.T) {
	root := resolvedTempDir(t)
	baseStreams := openStreamCount()
	baseGoroutines := runtime.NumGoroutine()

	stop := make(chan struct{})
	var writer sync.WaitGroup
	writer.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.WriteFile(filepath.Join(root, fmt.Sprintf("traffic%d.go", i%50)), []byte("x"), 0o644)
			time.Sleep(time.Millisecond)
		}
	})

	for cycle := range 50 {
		s, err := openFSEventStream(root)
		if err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		// Let a few batches arrive, and leave some undrained.
		deadline := time.After(30 * time.Millisecond)
	read:
		for {
			select {
			case <-s.batches:
			case <-deadline:
				break read
			}
		}
		var closers sync.WaitGroup
		for range 3 {
			closers.Go(s.close)
		}
		closers.Wait()
		// Nothing is handed over once close has returned.
		for len(s.batches) > 0 {
			<-s.batches
		}
		select {
		case b := <-s.batches:
			t.Fatalf("cycle %d: a batch of %d arrived after close", cycle, len(b))
		case <-time.After(20 * time.Millisecond):
		}
	}
	close(stop)
	writer.Wait()

	if n := openStreamCount(); n != baseStreams {
		t.Errorf("%d streams still registered after closing them all (baseline %d)", n, baseStreams)
	}
	if !waitFor(func() bool { return runtime.NumGoroutine() <= baseGoroutines+2 }, 5*time.Second) {
		t.Errorf("goroutines grew from %d to %d over 50 open/close cycles", baseGoroutines, runtime.NumGoroutine())
	}
}

// TestFSEventStream_ConcurrentOpenAndClose opens and closes streams from many
// goroutines at once, which races registry ids and the shared callback.
func TestFSEventStream_ConcurrentOpenAndClose(t *testing.T) {
	root := resolvedTempDir(t)
	base := openStreamCount()
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 10 {
				s, err := openFSEventStream(root)
				if err != nil {
					t.Errorf("goroutine %d, open %d: %v", g, i, err)
					return
				}
				_ = os.WriteFile(filepath.Join(root, fmt.Sprintf("g%d.go", g)), []byte("x"), 0o644)
				s.close()
			}
		})
	}
	wg.Wait()
	if n := openStreamCount(); n != base {
		t.Errorf("%d streams still registered (baseline %d)", n, base)
	}
}
