package cli

import (
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
