package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/golimpio/plumb/internal/config"
)

// globalConfigWatcher watches the directory holding the global config file and
// triggers store.Reload (debounced) when that file changes. It watches the
// DIRECTORY, not the file, because both plumb's own atomic Save (temp file →
// rename) and external editors (rename-replace) swap the inode — a file-level
// watch would miss the replacement. Events are filtered to the config file's
// basename and coalesced over a short debounce window so the burst a single
// save emits collapses to one reload.
//
// Self-trigger safety: Reload only reads the file and swaps the store's pointer;
// no subscriber writes the config back, so a reload never produces a new write
// event. The watcher therefore cannot loop on its own (or the daemon's) reloads.
//
// Concurrency: Run blocks until ctx is cancelled and is intended to run in its
// own goroutine for the daemon's lifetime.
type globalConfigWatcher struct {
	store    *config.Store
	dir      string
	base     string
	debounce time.Duration
}

// newGlobalConfigWatcher builds a watcher for the resolved global config path.
func newGlobalConfigWatcher(store *config.Store) *globalConfigWatcher {
	path := config.GlobalConfigPath()
	return &globalConfigWatcher{
		store:    store,
		dir:      filepath.Dir(path),
		base:     filepath.Base(path),
		debounce: 250 * time.Millisecond,
	}
}

// shouldReload reports whether an fsnotify event for the watched directory
// refers to the config file and represents a change worth reloading for.
func shouldReload(eventName, base string, op fsnotify.Op) bool {
	if filepath.Base(eventName) != base {
		return false
	}
	return op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) != 0
}

// Run watches the config directory until ctx is cancelled. A watcher that
// cannot be created or attached is logged and degraded to a no-op (the daemon
// still runs; the control-socket reload-config path remains available).
func (w *globalConfigWatcher) Run(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("creating config watcher: %w", err)
	}
	defer watcher.Close()

	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		return fmt.Errorf("creating config dir for watch: %w", err)
	}
	if err := watcher.Add(w.dir); err != nil {
		return fmt.Errorf("watching config dir %s: %w", w.dir, err)
	}
	slog.Info("daemon: watching global config for changes", "dir", w.dir, "file", w.base)

	timer := time.NewTimer(w.debounce)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			w.onEvent(event, timer)
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			slog.Warn("daemon: config watcher error", "err", err)
		case <-timer.C:
			w.reload()
		}
	}
}

// onEvent re-arms the debounce timer when an event refers to the config file.
// The stop-drain-reset dance restarts the window without leaking a stale fire.
func (w *globalConfigWatcher) onEvent(event fsnotify.Event, timer *time.Timer) {
	if !shouldReload(event.Name, w.base, event.Op) {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(w.debounce)
}

// reload re-reads the global config after the debounce window elapses.
func (w *globalConfigWatcher) reload() {
	if err := w.store.Reload(); err != nil {
		slog.Warn("daemon: reload after config file change failed", "err", err)
		return
	}
	slog.Info("daemon: global config reloaded from file change", "generation", w.store.Generation())
}
