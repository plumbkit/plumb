package topology

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sgtdi/fswatcher"
)

// watchEventFor builds a modify event for a workspace-relative path.
func watchEventFor(ws, rel string) fswatcher.WatchEvent {
	return fswatcher.WatchEvent{
		Path:  filepath.Join(ws, filepath.FromSlash(rel)),
		Types: []fswatcher.EventType{fswatcher.EventMod},
	}
}

// watcher_ignore_test.go covers the watcher's gitignore awareness. The
// important test here is TestFSWatcher_ThroughOSWatcher, which drives REAL
// filesystem events: the bug this file exists for is that the OS watcher's
// source exclusion swallowed every .gitignore event, so a design that reacts to
// ignore-file edits passes a direct unit test of handle() and never fires in
// production. Calling handle directly cannot see that.

// recordingSink is a concurrency-safe indexSink for tests that observe the
// consumer goroutine.
type recordingSink struct {
	mu       sync.Mutex
	enqueued []string
	resyncs  int
}

func (r *recordingSink) Enqueue(path string) {
	r.mu.Lock()
	r.enqueued = append(r.enqueued, filepath.ToSlash(path))
	r.mu.Unlock()
}

func (r *recordingSink) Resync() {
	r.mu.Lock()
	r.resyncs++
	r.mu.Unlock()
}

func (r *recordingSink) snapshot() ([]string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.enqueued), r.resyncs
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

func TestWatchExcludeRegexFor(t *testing.T) {
	ws := filepath.Clean(filepath.FromSlash("/home/u/.config/repo"))
	re, err := regexp.Compile(watchExcludeRegexFor(ws))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	p := func(rel string) string { return filepath.Join(ws, filepath.FromSlash(rel)) }

	// Delivered — these are the events the watcher must see.
	for _, rel := range []string{".gitignore", "src/.gitignore", ".ignore", "src/.ignore", "main.go", "src/a/b.go"} {
		if re.MatchString(p(rel)) {
			t.Errorf("%q excluded at the source; the watcher would never see it", rel)
		}
	}
	// Still excluded — the self-trigger guard and the heavy trees.
	for _, rel := range []string{".plumb/topology.db", ".git/index", "a/.git/HEAD", "vendor/x.go", "node_modules/p/i.js", "a/dist/o.js", "__pycache__/m.pyc"} {
		if !re.MatchString(p(rel)) {
			t.Errorf("%q not excluded at the source", rel)
		}
	}
	// The workspace's OWN ancestors must not exclude the whole tree: this
	// workspace lives under .config, and an unanchored dot branch matched it.
	if re.MatchString(p("main.go")) {
		t.Error("a workspace nested under a dot directory had every event excluded")
	}
	// The base name is matched too (fswatcher does both), so no bare component
	// may match on its own.
	for _, base := range []string{".gitignore", ".ignore", "main.go"} {
		if re.MatchString(base) {
			t.Errorf("base name %q matched the exclusion", base)
		}
	}
}

// TestFSWatcher_ThroughOSWatcher exercises create / delete / rename and a
// .gitignore edit as real filesystem events, through the platform watcher and
// the consumer goroutine. Every assertion below is one a handle()-only test
// would pass while production did nothing.
func TestFSWatcher_ThroughOSWatcher(t *testing.T) {
	// EvalSymlinks because macOS hands /var/folders/... from t.TempDir() and
	// reports /private/var/folders/... in events; the workspace must be the
	// resolved form or filepath.Rel escapes and every event is dropped.
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	write := func(rel, content string) {
		t.Helper()
		abs := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	sink := &recordingSink{}
	fw, err := newFSWatcher(ws, sink, nil)
	if err != nil {
		t.Fatalf("newFSWatcher: %v", err)
	}
	fw.Start()
	t.Cleanup(fw.Stop)

	sawEnqueue := func(rel string) func() bool {
		return func() bool {
			enq, _ := sink.snapshot()
			return slices.Contains(enq, rel)
		}
	}

	// Sanity, and the warm-up the platform watcher needs: FSEvents and inotify
	// both take a moment to establish, and a write that lands before the stream
	// exists produces no event ever. Rewriting until one arrives is what makes
	// the assertions below reliable rather than a race against startup. A
	// sandbox that cannot watch at all is reported as a skip here and nowhere
	// else — every later assertion then runs for real.
	warm := waitFor(func() bool {
		write("main.go", "package main\n")
		return waitFor(sawEnqueue("main.go"), 500*time.Millisecond)
	}, 30*time.Second)
	if !warm {
		t.Skip("platform watcher delivered no events in this environment")
	}

	// 1. A .gitignore write must reach the watcher and escalate to a resync.
	//    This is the assertion the source exclusion used to make impossible.
	_, beforeResyncs := sink.snapshot()
	write(".gitignore", "out/\n*.log\n")
	if !waitFor(func() bool { _, n := sink.snapshot(); return n > beforeResyncs }, 20*time.Second) {
		t.Fatal("writing .gitignore produced no Resync: the OS watcher never delivered the event")
	}

	// 2. A file under a newly ignored directory must NOT be enqueued. The
	//    sentinel is the positive control: once it lands, the ignored file's
	//    event has had at least as long to arrive.
	write("out/generated.go", "package out\n")
	write("sentinel.go", "package main\n")
	if !waitFor(sawEnqueue("sentinel.go"), 20*time.Second) {
		t.Fatal("sentinel.go never enqueued")
	}
	if enq, _ := sink.snapshot(); slices.Contains(enq, "out/generated.go") {
		t.Errorf("gitignored out/generated.go was enqueued: %v", enq)
	}
	// An ignored file at any depth, and one matched by a name rule.
	write("out/deep/nested.go", "package deep\n")
	write("noisy.log", "x\n")
	write("sentinel2.go", "package main\n")
	if !waitFor(sawEnqueue("sentinel2.go"), 20*time.Second) {
		t.Fatal("sentinel2.go never enqueued")
	}
	enq, _ := sink.snapshot()
	for _, unwanted := range []string{"out/deep/nested.go", "noisy.log"} {
		if slices.Contains(enq, unwanted) {
			t.Errorf("ignored %q was enqueued: %v", unwanted, enq)
		}
	}

	// 3. Delete: a tracked file's removal must still enqueue (Enqueue routes a
	//    missing path to a delete).
	if err := os.Remove(filepath.Join(ws, "main.go")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	countMain := func() int {
		e, _ := sink.snapshot()
		n := 0
		for _, p := range e {
			if p == "main.go" {
				n++
			}
		}
		return n
	}
	before := countMain()
	if !waitFor(func() bool { return countMain() > before }, 20*time.Second) {
		t.Error("deleting main.go produced no enqueue")
	}

	// 4. Rename: the new name must be enqueued.
	if err := os.Rename(filepath.Join(ws, "sentinel.go"), filepath.Join(ws, "renamed.go")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if !waitFor(sawEnqueue("renamed.go"), 20*time.Second) {
		t.Error("renaming sentinel.go -> renamed.go produced no enqueue for the new name")
	}

	// 5. Removing the rule re-includes the tree: the resync that the edit
	//    triggers is what re-indexes it, and the cache must not answer stale.
	_, beforeResyncs = sink.snapshot()
	write(".gitignore", "*.log\n")
	if !waitFor(func() bool { _, n := sink.snapshot(); return n > beforeResyncs }, 20*time.Second) {
		t.Fatal("rewriting .gitignore produced no Resync")
	}
	write("out/generated.go", "package out\n// changed\n")
	if !waitFor(sawEnqueue("out/generated.go"), 20*time.Second) {
		t.Error("out/generated.go still treated as ignored after the rule was removed: the ignore cache went stale")
	}
}

// TestFSWatcher_IgnoredPathTestsAncestors pins the contract obligation that
// ignore.Stack.IsIgnored puts on an out-of-walk-order caller: `build/` is
// directory-only, so asked about build/x/y.go directly it answers false. The
// watcher has to test the ancestors itself.
func TestFSWatcher_IgnoredPathTestsAncestors(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, ".gitignore"), []byte("gen/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(ws, "gen", "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(deep, "y.go")
	if err := os.WriteFile(target, []byte("package b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fw := &fsWatcher{workspace: ws, sink: &recordingSink{}}

	st := fw.stackFor(ws)
	if st.IsIgnored(target, false) {
		t.Fatal("premise broken: a root-level stack asked directly about gen/a/b/y.go should answer false")
	}
	if !fw.ignoredPath(target) {
		t.Error("ignoredPath(gen/a/b/y.go) = false; the ancestor chain was not tested")
	}
	if fw.ignoredPath(filepath.Join(ws, "main.go")) {
		t.Error("ignoredPath(main.go) = true")
	}
}

// TestFSWatcher_HandleIgnoreFileUnderSkippedDir: an ignore file inside a tree
// the indexer never walks changes nothing, so it must not cost a full resync.
func TestFSWatcher_HandleIgnoreFileUnderSkippedDir(t *testing.T) {
	ws := t.TempDir()
	sink := &recordingSink{}
	fw := &fsWatcher{workspace: ws, sink: sink}
	fw.handle(watchEventFor(ws, "vendor/.gitignore"))
	if _, n := sink.snapshot(); n != 0 {
		t.Errorf("vendor/.gitignore triggered %d resyncs, want 0", n)
	}
	fw.handle(watchEventFor(ws, ".gitignore"))
	if _, n := sink.snapshot(); n != 1 {
		t.Errorf("root .gitignore triggered %d resyncs, want 1", n)
	}
}

// TestFSWatcher_ExcludePatternsFilterEnqueue: exclude_patterns applies to the
// watcher as well as the resync walk, or a write to an excluded file re-indexes
// it and the row survives until the next resync.
func TestFSWatcher_ExcludePatternsFilterEnqueue(t *testing.T) {
	ws := t.TempDir()
	sink := &recordingSink{}
	fw := &fsWatcher{workspace: ws, sink: sink, excludePatterns: []string{"third_party/**"}}
	fw.handle(watchEventFor(ws, "third_party/lib/a.go"))
	fw.handle(watchEventFor(ws, "main.go"))
	enq, _ := sink.snapshot()
	if !slices.Equal(enq, []string{"main.go"}) {
		t.Errorf("enqueued = %v, want [main.go]", enq)
	}
	if strings.Contains(strings.Join(enq, ","), "third_party") {
		t.Error("excluded path enqueued")
	}
}
