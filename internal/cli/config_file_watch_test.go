package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/fsnotify/fsnotify"
)

// newTestFileWatch builds a configFileWatch on path with a real OS watcher,
// closed when the test ends.
func newTestFileWatch(t *testing.T, path string) *configFileWatch {
	t.Helper()
	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	c := &configFileWatch{path: path, newWatcher: fsnotify.NewWatcher, watcher: w}
	t.Cleanup(func() { closeFSWatcher(c.watcher) })
	return c
}

func TestConfigFileWatch_OnEventFiltersToTheFile(t *testing.T) {
	tests := []struct {
		name     string
		other    bool
		op       fsnotify.Op
		want     bool
		detaches bool
	}{
		{"write", false, fsnotify.Write, true, false},
		{"create", false, fsnotify.Create, true, false},
		{"rename away", false, fsnotify.Rename, true, true},
		{"remove", false, fsnotify.Remove, true, true},
		{"chmod only", false, fsnotify.Chmod, false, false},
		{"another file", true, fsnotify.Write, false, false},
		{"another file removed", true, fsnotify.Remove, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			c := newTestFileWatch(t, path)
			if !c.attach() {
				t.Fatal("attach of an existing file reported no attach")
			}
			name := path
			if tt.other {
				name = filepath.Join(dir, "collab.db-wal")
			}
			if got := c.onEvent(fsnotify.Event{Name: name, Op: tt.op}); got != tt.want {
				t.Errorf("onEvent(%s %v) = %v, want %v", filepath.Base(name), tt.op, got, tt.want)
			}
			if c.attached == tt.detaches {
				t.Errorf("attached = %v after %v, want %v", c.attached, tt.op, !tt.detaches)
			}
		})
	}
}

// TestConfigFileWatch_CheckAttachesAndNoticesSwap pins the tick, which stands
// in for what the directory watch used to see: a file appearing where there
// was none, and the name moving to another inode.
func TestConfigFileWatch_CheckAttachesAndNoticesSwap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	c := newTestFileWatch(t, path)

	if c.check() || c.attached {
		t.Fatal("check attached a file that does not exist")
	}
	if err := os.WriteFile(path, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !c.check() || !c.attached {
		t.Fatal("check did not attach a file that appeared")
	}
	if c.check() {
		t.Fatal("check reported a change on an unchanged file")
	}

	// A swap that raises nothing on the watched inode: the old file keeps a
	// second link, so the rename over it is not a delete of that inode.
	if err := os.Link(path, filepath.Join(dir, "keep")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	tmp := filepath.Join(dir, "config.toml.tmp")
	if err := os.WriteFile(tmp, []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if !c.check() || c.attached {
		t.Fatal("check did not notice config.toml now names another inode")
	}
	if !c.check() || !c.attached {
		t.Fatal("check did not re-attach to the new inode")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if !c.check() || c.attached {
		t.Fatal("check did not notice config.toml was deleted")
	}
}

// TestConfigFileWatch_ReattachesAcrossManySwaps pins why detach replaces the
// OS watcher instead of calling fsnotify's Remove: on kqueue, Remove of a file
// whose name just moved to a new inode can fail half-way and leave the next Add
// with EBADF. The failure depends on fsnotify's reader goroutine racing the
// Remove, so no in-process test makes it deterministic: one swap catches it
// rarely, and a hundred swaps with the old inode kept alive killed a
// Remove-based detach in 3 of 5 runs when measured (2026-10-09).
func TestConfigFileWatch_ReattachesAcrossManySwaps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("v0"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newTestFileWatch(t, path)
	if !c.attach() {
		t.Fatal("initial attach failed")
	}
	tmp := filepath.Join(dir, "config.toml.tmp")
	for i := range 100 {
		// Keep the outgoing inode alive under a second name, as the deterministic
		// swap above does: Remove then meets a watch whose file still exists.
		if err := os.Link(path, filepath.Join(dir, fmt.Sprintf("keep-%d", i))); err != nil {
			t.Skipf("hard links unavailable: %v", err)
		}
		if err := os.WriteFile(tmp, []byte("v"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
		if !c.check() || c.attached {
			t.Fatalf("swap %d: the new inode was not noticed", i)
		}
		if !c.check() || !c.attached {
			t.Fatalf("swap %d: re-attach failed; a half-failed watcher Remove leaves the next Add with EBADF", i)
		}
	}
}
