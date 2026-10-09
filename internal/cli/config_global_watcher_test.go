package cli

import (
	"path/filepath"
	"testing"
)

func TestNewGlobalConfigWatcher_ResolvesPaths(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg-test")
	w := newGlobalConfigWatcher(nil)
	if w.base != "config.toml" {
		t.Errorf("base = %q, want config.toml", w.base)
	}
	if filepath.Base(w.dir) != "plumb" {
		t.Errorf("dir = %q, want a .../plumb directory", w.dir)
	}
	if w.debounce <= 0 {
		t.Errorf("debounce = %v, want a positive window", w.debounce)
	}
}
