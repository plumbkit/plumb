package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
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

// TestGlobalConfigWatcher_DeleteKeepsLoadedConfig pins the contract the file
// watch must keep: deleting (or renaming away) the global config.toml does NOT
// reload, because a missing global file resolves to compiled defaults, which
// would silently reset a live daemon. Only a file that exists is reloaded.
func TestGlobalConfigWatcher_DeleteKeepsLoadedConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	gw := newGlobalConfigWatcher(store)
	gw.debounce = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gw.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	if err := config.Save(func(c *config.Config) { c.Edits.Strict = true }); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Positive control: the watcher does reload a file that exists.
	for deadline := time.Now().Add(10 * time.Second); !store.Current().Edits.Strict; {
		if time.Now().After(deadline) {
			t.Fatal("the watcher never reloaded strict=true, so the delete check below would prove nothing")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := os.Remove(filepath.Join(gw.dir, gw.base)); err != nil {
		t.Fatal(err)
	}
	// Longer than the debounce plus two stat ticks: every path that could
	// react to the delete has had its chance.
	time.Sleep(2*configFileTick + 10*gw.debounce)
	if !store.Current().Edits.Strict {
		t.Fatal("deleting the global config.toml reloaded it: the live config was reset to defaults")
	}
}
