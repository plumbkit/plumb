package cli

import (
	"path/filepath"
	"testing"

	"github.com/fsnotify/fsnotify"
)

func TestShouldReload(t *testing.T) {
	const base = "config.toml"
	tests := []struct {
		name  string
		event string
		op    fsnotify.Op
		want  bool
	}{
		{"write to config", base, fsnotify.Write, true},
		{"create config", base, fsnotify.Create, true},
		{"rename onto config", base, fsnotify.Rename, true},
		{"full path write", filepath.Join("/home/u/.config/plumb", base), fsnotify.Write, true},
		{"chmod only", base, fsnotify.Chmod, false},
		{"remove", base, fsnotify.Remove, false},
		{"different file", "other.toml", fsnotify.Write, false},
		{"temp staging file", ".config-123.toml.tmp", fsnotify.Create, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldReload(tt.event, base, tt.op); got != tt.want {
				t.Errorf("shouldReload(%q, %q, %v) = %v, want %v", tt.event, base, tt.op, got, tt.want)
			}
		})
	}
}

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
