//go:build integration

package cli

import (
	"context"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
)

// TestGlobalConfigWatcher_ReloadsOnFileChange exercises the real fsnotify path:
// an atomic config.Save into the watched directory must trigger store.Reload.
// Gated behind the integration tag because real filesystem events are timing
// sensitive.
func TestGlobalConfigWatcher_ReloadsOnFileChange(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	if store.Current().Edits.Strict {
		t.Fatal("expected non-strict default before the test writes config")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = newGlobalConfigWatcher(store).Run(ctx) }()
	time.Sleep(150 * time.Millisecond) // let the directory watch attach

	if err := config.Save(func(c *config.Config) { c.Edits.Strict = true }); err != nil {
		t.Fatalf("Save: %v", err)
	}

	deadline := time.After(3 * time.Second)
	for !store.Current().Edits.Strict {
		select {
		case <-deadline:
			t.Fatal("store did not pick up strict=true within 3s of the config write")
		case <-time.After(50 * time.Millisecond):
		}
	}
}
