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
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = newGlobalConfigWatcher(store).Run(ctx)
	}()
	// Wait for Run to close its watcher before the temp dir it watches is
	// removed: deleting a watched tree while fsnotify's kqueue Close runs can
	// close a descriptor twice and break whatever reuses the number (see
	// closeFSWatcher). Deferred, so it runs before the t.TempDir clean-up.
	defer func() {
		cancel()
		<-done
	}()
	time.Sleep(150 * time.Millisecond) // let Run start; the stat tick attaches the file once Save creates it

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
