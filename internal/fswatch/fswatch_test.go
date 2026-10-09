package fswatch

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestExcludeDirsRegex(t *testing.T) {
	// The root lives under a dot-prefixed directory, the case an unanchored
	// pattern turned into "exclude everything".
	root := filepath.Clean(filepath.FromSlash("/home/u/.config/repo"))
	re := regexp.MustCompile(ExcludeDirsRegex(root, "vendor", "build"))
	// excluded applies the pattern as the backends do: to the full path and to
	// the base name.
	excluded := func(rel string) bool {
		p := filepath.Join(root, filepath.FromSlash(rel))
		return re.MatchString(p) || re.MatchString(filepath.Base(p))
	}
	for _, rel := range []string{"main.go", "src/a/b.go", ".gitignore", "src/.gitignore", ".env", "vendored.go", "src/builder/x.go", "build.go"} {
		if excluded(rel) {
			t.Errorf("%q excluded; it must be delivered", rel)
		}
	}
	for _, rel := range []string{".plumb/collab.db", ".plumb/collab.db-wal", ".git/index", "a/.cache/x", "vendor/x.go", "a/vendor/b/c.go", "build/o", "a/build"} {
		if !excluded(rel) {
			t.Errorf("%q not excluded", rel)
		}
	}
	// No bare base name can match: the pattern needs the root prefix.
	for _, base := range []string{".plumb", ".git", "vendor", "build", ".gitignore", "main.go"} {
		if re.MatchString(base) {
			t.Errorf("base name %q matched on its own", base)
		}
	}
	// The root itself, and a sibling that merely shares its prefix, are not
	// excluded.
	if re.MatchString(root) {
		t.Error("the root itself matched")
	}
	if re.MatchString(root + "-other" + string(filepath.Separator) + filepath.Join("vendor", "x.go")) {
		t.Error("a sibling of the root sharing its name prefix matched")
	}

	dotOnly := regexp.MustCompile(ExcludeDirsRegex(root))
	if !dotOnly.MatchString(filepath.Join(root, ".git", "HEAD")) || dotOnly.MatchString(filepath.Join(root, "vendor", "x.go")) {
		t.Error("with no names, only dot directories should be excluded")
	}
}

func TestNew_RejectsNegativeCooldown(t *testing.T) {
	if _, err := New(t.TempDir(), Options{Cooldown: -time.Second}); err == nil {
		t.Fatal("New accepted a negative cooldown")
	}
}

// TestWatcher_CloseIsIdempotentAndClosesChannels: Close may be called twice,
// from different goroutines, and leaves both channels closed so a consumer
// ranging over them ends.
func TestWatcher_CloseIsIdempotentAndClosesChannels(t *testing.T) {
	w, err := New(t.TempDir(), Options{Cooldown: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(w.Close)
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return")
	}
	if _, ok := <-w.Events(); ok {
		t.Error("Events still open after Close")
	}
	if _, ok := <-w.Lost(); ok {
		t.Error("Lost still open after Close")
	}
}

// collector records what a Watcher delivers.
type collector struct {
	mu   sync.Mutex
	evs  []Event
	lost int
}

func collect(t *testing.T, w *Watcher) *collector {
	t.Helper()
	c := &collector{}
	var wg sync.WaitGroup
	wg.Go(func() {
		for ev := range w.Events() {
			c.mu.Lock()
			c.evs = append(c.evs, ev)
			c.mu.Unlock()
		}
	})
	wg.Go(func() {
		for range w.Lost() {
			c.mu.Lock()
			c.lost++
			c.mu.Unlock()
		}
	})
	t.Cleanup(func() { w.Close(); wg.Wait() })
	return c
}

func (c *collector) snapshot() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.evs)
}

// count reports how many delivered events name path and carry any bit of op
// (0 matches every op).
func (c *collector) count(path string, op Op) int {
	n := 0
	for _, ev := range c.snapshot() {
		if ev.Path == path && (op == 0 || ev.Op.Has(op)) {
			n++
		}
	}
	return n
}

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

// warmUp rewrites a file until its event arrives: a stream established after a
// write never reports it, so every later assertion would race startup.
func warmUp(t *testing.T, c *collector, root string) {
	t.Helper()
	p := filepath.Join(root, "warm.go")
	ok := waitFor(func() bool {
		if err := os.WriteFile(p, []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return waitFor(func() bool { return c.count(p, 0) > 0 }, 500*time.Millisecond)
	}, 30*time.Second)
	if !ok {
		t.Fatal("the watcher delivered no event at all")
	}
}

// TestWatcher_RealEvents drives every change class plumb's consumers rely on
// through the real platform backend. The root is t.TempDir() as returned, NOT
// symlink-resolved: on macOS that is /var/..., which FSEvents reports as
// /private/var/..., so every path assertion below also checks that events are
// rebased onto the caller's spelling of the root, deleted and renamed-away
// paths included.
func TestWatcher_RealEvents(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, Options{Cooldown: 50 * time.Millisecond, ExcludeRegex: ExcludeDirsRegex(root, "vendor")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := collect(t, w)
	warmUp(t, c, root)
	at := func(rel string) string { return filepath.Join(root, filepath.FromSlash(rel)) }
	write := func(rel, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(at(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(at(rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	expect := func(what, rel string, op Op) {
		t.Helper()
		if !waitFor(func() bool { return c.count(at(rel), op) > 0 }, 15*time.Second) {
			t.Errorf("%s: no event for %s with op %05b; got %v", what, rel, op, c.snapshot())
		}
	}

	write("a/b/c/new.go", "package c\n")
	expect("create in a brand-new nested directory", "a/b/c/new.go", 0)

	before := c.count(at("a/b/c/new.go"), 0)
	time.Sleep(200 * time.Millisecond)
	f, err := os.OpenFile(at("a/b/c/new.go"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("// appended\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if !waitFor(func() bool { return c.count(at("a/b/c/new.go"), 0) > before }, 15*time.Second) {
		t.Error("in-place append: no further event")
	}

	write("atomic.go", "package main\n")
	expect("create", "atomic.go", 0)
	before = c.count(at("atomic.go"), 0)
	write("atomic.go.tmp", "package main\n// v2\n")
	if err := os.Rename(at("atomic.go.tmp"), at("atomic.go")); err != nil {
		t.Fatal(err)
	}
	if !waitFor(func() bool { return c.count(at("atomic.go"), 0) > before }, 15*time.Second) {
		t.Error("atomic replace: no event for the target")
	}

	write("old.go", "package main\n")
	expect("create", "old.go", 0)
	if err := os.Rename(at("old.go"), at("renamed.go")); err != nil {
		t.Fatal(err)
	}
	expect("rename, new name", "renamed.go", 0)
	if !waitFor(func() bool { return c.count(at("old.go"), Rename|Remove) > 0 }, 15*time.Second) {
		t.Errorf("rename, old name: no rename/remove event under the caller's root; got %v", c.snapshot())
	}

	if err := os.Remove(at("renamed.go")); err != nil {
		t.Fatal(err)
	}
	expect("delete", "renamed.go", Remove)

	// The same name created and removed many times, then created for good: a
	// backend that registers per file can leave a dead watch here and never
	// report that name again (fsnotify#783).
	for range 200 {
		_ = os.WriteFile(at("churn.go"), []byte("x"), 0o644)
		_ = os.Remove(at("churn.go"))
	}
	time.Sleep(300 * time.Millisecond)
	before = c.count(at("churn.go"), 0)
	write("churn.go", "package main\n")
	if !waitFor(func() bool { return c.count(at("churn.go"), 0) > before }, 15*time.Second) {
		t.Error("create after create/unlink churn: no event")
	}

	// Exclusion, with a sentinel as the positive control: once it lands, the
	// excluded writes have had at least as long to arrive.
	write("vendor/x.go", "package vendor\n")
	write(".hidden/y.go", "package hidden\n")
	write("sentinel.go", "package main\n")
	expect("sentinel", "sentinel.go", 0)
	for _, rel := range []string{"vendor/x.go", ".hidden/y.go"} {
		if n := c.count(at(rel), 0); n != 0 {
			t.Errorf("excluded %s delivered %d event(s)", rel, n)
		}
	}

	for _, ev := range c.snapshot() {
		if !strings.HasPrefix(ev.Path, root+string(filepath.Separator)) {
			t.Errorf("event path %q is not under the root as the caller spelled it (%s)", ev.Path, root)
		}
	}
}
