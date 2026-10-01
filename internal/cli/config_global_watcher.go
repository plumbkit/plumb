package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/plumbkit/plumb/internal/config"
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
	// newWatcher builds each OS watcher: fsnotify.NewWatcher, or a test's
	// wrapper that captures each watcher it builds.
	newWatcher func() (*fsnotify.Watcher, error)
	// recreateInterval is watcherRecreateInterval; tests widen it so "lost
	// again straight away" does not depend on scheduling.
	recreateInterval time.Duration
	// testErrs is a test seam: an error sent on it reaches Run exactly as if
	// the OS watcher had reported it. Nil in production (a nil channel never
	// fires in a select).
	testErrs chan error
}

// newGlobalConfigWatcher builds a watcher for the resolved global config path.
func newGlobalConfigWatcher(store *config.Store) *globalConfigWatcher {
	path := config.GlobalConfigPath()
	return &globalConfigWatcher{
		store:      store,
		dir:        filepath.Dir(path),
		base:       filepath.Base(path),
		debounce:   250 * time.Millisecond,
		newWatcher: fsnotify.NewWatcher,

		recreateInterval: watcherRecreateInterval,
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
// still runs; the control-socket reload-config path remains available). A
// watcher that loses its own descriptor at runtime is recreated in place (see
// recreateLost). Run returns only after its OS watcher is closed.
func (w *globalConfigWatcher) Run(ctx context.Context) error {
	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		return fmt.Errorf("creating config dir for watch: %w", err)
	}
	watcher, err := w.open()
	if err != nil {
		return err
	}
	// A closure, not a plain defer: recreateLost swaps the watcher.
	defer func() { closeFSWatcher(watcher) }()
	slog.Info("daemon: watching global config for changes", "dir", w.dir, "file", w.base)

	timer := time.NewTimer(w.debounce)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	var recreatedAt time.Time
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
			if watcher, err = w.onError(watcher, err, &recreatedAt, timer); err != nil {
				return err
			}
		case err := <-w.testErrs:
			if watcher, err = w.onError(watcher, err, &recreatedAt, timer); err != nil {
				return err
			}
		case <-timer.C:
			w.reload()
		}
	}
}

// onError handles one watcher error and returns the watcher to carry on
// with. A lost watcher is recreated (recreateLost) and a reload scheduled,
// because the file may have changed while it was blind; a non-nil error
// means recreating failed or was refused, and ends Run. Any other error is
// only logged, as it always was.
func (w *globalConfigWatcher) onError(watcher *fsnotify.Watcher, err error, recreatedAt *time.Time, timer *time.Timer) (*fsnotify.Watcher, error) {
	if !fsWatcherLost(err) {
		slog.Warn("daemon: config watcher error", "err", err)
		return watcher, nil
	}
	next, err := w.recreateLost(watcher, err, *recreatedAt)
	if err != nil {
		return nil, err
	}
	*recreatedAt = time.Now()
	rearmProjectTimer(timer, w.debounce)
	return next, nil
}

// open creates the OS watcher on the config directory. An attach that fails
// because a descriptor was closed underneath it (fsWatcherLost) says nothing
// about the directory, so it gets one immediate second attempt.
func (w *globalConfigWatcher) open() (*fsnotify.Watcher, error) {
	watcher, err := w.openOnce()
	if fsWatcherLost(err) {
		watcher, err = w.openOnce()
	}
	return watcher, err
}

func (w *globalConfigWatcher) openOnce() (*fsnotify.Watcher, error) {
	watcher, err := w.newWatcher()
	if err != nil {
		return nil, fmt.Errorf("creating config watcher: %w", err)
	}
	if err := watcher.Add(w.dir); err != nil {
		closeFSWatcher(watcher)
		return nil, fmt.Errorf("watching config dir %s: %w", w.dir, err)
	}
	return watcher, nil
}

// recreateLost replaces a watcher that lost its own descriptor, which would
// otherwise never deliver another event while its reader spins on the same
// error. The old watcher is always closed first — before the replacement
// opens, so the replacement cannot be handed the number the old reader closes
// on its way out. A watcher lost again within recreateInterval of the
// last recreate is not a one-off, so that returns an error instead: the
// global hot reload stops, as it does when the watcher cannot start.
func (w *globalConfigWatcher) recreateLost(old *fsnotify.Watcher, cause error, last time.Time) (*fsnotify.Watcher, error) {
	closeFSWatcher(old)
	if !last.IsZero() && time.Since(last) < w.recreateInterval {
		return nil, fmt.Errorf("config watcher lost again straight after a recreate: %w", cause)
	}
	next, err := w.open()
	if err != nil {
		return nil, fmt.Errorf("recreating lost config watcher: %w", err)
	}
	slog.Warn("daemon: config watcher lost its OS handle — recreated", "err", cause)
	return next, nil
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
