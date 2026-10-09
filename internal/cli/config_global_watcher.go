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

// globalConfigWatcher watches the global config file and triggers store.Reload
// (debounced) when it changes. It watches the FILE, never its directory: on a
// fresh macOS install that directory is also the data directory holding
// stats.db, session_state.db, history.db and collab-xproject.db, and on kqueue
// a directory watch opens every file in it, which strips the daemon's SQLite
// locks on them (PLAN-485, see configFileWatch). Both plumb's own atomic Save
// (temp file → rename) and external editors (rename-replace) swap the inode;
// the Remove or Rename that raises on the old inode detaches the watch and the
// debounce fire re-attaches it to the new file before reloading. A config file
// created where there was none is attached by a one-second stat tick. Bursts
// coalesce over a short debounce window so a single save reloads once.
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

// Run watches the config file until ctx is cancelled. A watcher that cannot be
// created is logged and degraded to a no-op (the daemon still runs; the
// control-socket reload-config path remains available). A watcher that loses
// its own descriptor at runtime is recreated in place (see recreateLost). Run
// returns only after its OS watcher has been closed and its reader has stopped
// delivering (closeFSWatcher).
func (w *globalConfigWatcher) Run(ctx context.Context) error {
	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		return fmt.Errorf("creating config dir for watch: %w", err)
	}
	watcher, err := w.newWatcher()
	if err != nil {
		return fmt.Errorf("creating config watcher: %w", err)
	}
	cfg := configFileWatch{path: filepath.Join(w.dir, w.base), newWatcher: w.newWatcher, watcher: watcher}
	// A closure, not a plain defer: onError swaps the watcher.
	defer func() { closeFSWatcher(cfg.watcher) }()
	cfg.attach()
	slog.Info("daemon: watching global config for changes", "dir", w.dir, "file", w.base)

	timer := time.NewTimer(w.debounce)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	tick := time.NewTicker(configFileTick)
	defer tick.Stop()

	var recreatedAt time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-cfg.events():
			if !ok {
				return nil
			}
			cfg.handleEvent(event, timer, w.debounce)
		case err, ok := <-cfg.errs():
			if !ok {
				return nil
			}
			if err := w.onError(&cfg, err, &recreatedAt, timer); err != nil {
				return err
			}
		case err := <-w.testErrs:
			if err := w.onError(&cfg, err, &recreatedAt, timer); err != nil {
				return err
			}
		case <-tick.C:
			cfg.handleTick(timer, w.debounce)
		case <-timer.C:
			w.onSettled(&cfg)
		}
	}
}

// onError handles one watcher error. A lost watcher is recreated
// (recreateLost), its file watch re-attached, and a reload scheduled, because
// the file may have changed while it was blind; a non-nil error means
// recreating failed or was refused, and ends Run. Any other error is only
// logged, as it always was.
func (w *globalConfigWatcher) onError(cfg *configFileWatch, err error, recreatedAt *time.Time, timer *time.Timer) error {
	if !fsWatcherLost(err) {
		slog.Warn("daemon: config watcher error", "err", err)
		return nil
	}
	next, err := w.recreateLost(cfg.watcher, err, *recreatedAt)
	if err != nil {
		cfg.watcher = nil // recreateLost closed it
		return err
	}
	*cfg = configFileWatch{path: cfg.path, newWatcher: w.newWatcher, watcher: next}
	cfg.attach()
	*recreatedAt = time.Now()
	rearmProjectTimer(timer, w.debounce)
	return nil
}

// recreateLost replaces a watcher that lost its own descriptor, which would
// otherwise never deliver another event while its reader spins on the same
// error. The old watcher is always closed first — before the replacement
// opens — which shrinks the window in which the replacement could be handed
// the number the old reader closes on its way out; it cannot close it, because
// that reader's final closes can trail closeFSWatcher by microseconds. A
// watcher lost again within recreateInterval of the last recreate is not a
// one-off, so that returns an error instead: the global hot reload stops, as
// it does when the watcher cannot start.
func (w *globalConfigWatcher) recreateLost(old *fsnotify.Watcher, cause error, last time.Time) (*fsnotify.Watcher, error) {
	closeFSWatcher(old)
	if !last.IsZero() && time.Since(last) < w.recreateInterval {
		return nil, fmt.Errorf("config watcher lost again straight after a recreate: %w", cause)
	}
	next, err := w.newWatcher()
	if err != nil {
		return nil, fmt.Errorf("recreating lost config watcher: %w", err)
	}
	slog.Warn("daemon: config watcher lost its OS handle — recreated", "err", cause)
	return next, nil
}

// onSettled runs when the debounce window elapses. It re-attaches first, so
// the reload reads the file with the watch already live. A file that is gone
// is not reloaded: a missing global config resolves to compiled defaults,
// which a reload cannot improve on, so a delete keeps what was loaded, as it
// always has.
func (w *globalConfigWatcher) onSettled(cfg *configFileWatch) {
	cfg.attach()
	if cfg.attached {
		w.reload()
	}
}

// reload re-reads the global config after the debounce window elapses.
func (w *globalConfigWatcher) reload() {
	if err := w.store.Reload(); err != nil {
		slog.Warn("daemon: reload after config file change failed", "err", err)
		return
	}
	slog.Info("daemon: global config reloaded from file change", "generation", w.store.Generation())
}
