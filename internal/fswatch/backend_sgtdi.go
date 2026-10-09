//go:build linux || windows

package fswatch

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/sgtdi/fswatcher"
)

// rootRetryMin and rootRetryMax bound how often a lost root is looked for.
const (
	rootRetryMin = 250 * time.Millisecond
	rootRetryMax = 10 * time.Second
)

// sgtdiRun is one running sgtdi watcher.
type sgtdiRun struct {
	src     fswatcher.Watcher
	cancel  context.CancelFunc
	stopped chan struct{} // closed when Watch returns
	err     error         // Watch's result; read only after stopped is closed
}

// startSgtdi starts sgtdi/fswatcher over root: inotify on Linux,
// ReadDirectoryChangesW on Windows. Neither opens the files it watches. sgtdi
// debounces and applies the exclusion itself, exactly as plumb configured it
// before this package existed.
func startSgtdi(root string, opts Options) (*sgtdiRun, error) {
	cfg := []fswatcher.WatcherOpt{
		fswatcher.WithSeverity(fswatcher.SeverityNone), // plumb does its own logging
		fswatcher.WithCooldown(opts.Cooldown),
		fswatcher.WithPath(root, fswatcher.WithDepth(fswatcher.WatchNested)),
	}
	if opts.ExcludeRegex != "" {
		cfg = append(cfg, fswatcher.WithExcRegex(opts.ExcludeRegex))
	}
	src, err := fswatcher.New(cfg...)
	if err != nil {
		return nil, fmt.Errorf("fswatch: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &sgtdiRun{src: src, cancel: cancel, stopped: make(chan struct{})}
	go func() {
		r.err = src.Watch(ctx)
		close(r.stopped)
	}()
	return r, nil
}

// close stops the run and waits for its Watch to return.
func (r *sgtdiRun) close() {
	r.cancel()
	r.src.Close()
	<-r.stopped
}

func (w *Watcher) startBackend(opts Options) error {
	run, err := startSgtdi(w.root, opts)
	if err != nil {
		return err
	}
	w.wg.Go(func() { w.superviseSgtdi(opts, run) })
	return nil
}

// runEnd is why pumpSgtdi returned.
type runEnd int

const (
	endClosed   runEnd = iota // the Watcher was closed
	endRootLost               // the root was removed or renamed away
	endFailed                 // the backend stopped on its own, root still in place
)

// superviseSgtdi pumps one sgtdi run after another until the Watcher closes.
//
// inotify watches inodes, so when the root directory is renamed away or removed
// the watch follows the old directory or dies, and a directory recreated at the
// root path is never watched: the watcher would go silently dead. Instead a lost
// root asks for a reconcile, waits for the root to be a directory again, starts
// a fresh run, and asks for another reconcile, which covers whatever was made in
// it before the new watch was in place.
func (w *Watcher) superviseSgtdi(opts Options, run *sgtdiRun) {
	for {
		end := pumpSgtdi(w, run.src.Events(), run.src.Dropped(), run.stopped)
		run.close()
		switch end {
		case endClosed:
			return
		case endFailed:
			// Not a lost root, so restarting would most likely fail the same way
			// (an inotify watch limit, say) and turn into a reconcile loop. The
			// Watcher is degraded for good: say so on Failed, so a consumer can
			// fall back to periodic reconciliation, and on Lost for the changes
			// already missed.
			slog.Warn("fswatch: watcher stopped; changes will not be seen until it is restarted", "root", w.root, "err", run.err)
			close(w.failed) // once: the supervisor returns here and never runs again
			w.signalLost()
			return
		}
		w.signalLost()
		if run = w.restartSgtdi(opts); run == nil {
			return
		}
		w.signalLost()
	}
}

// restartSgtdi waits for the root to be a directory again and starts a new run,
// backing off between attempts. It returns nil once the Watcher is closed.
func (w *Watcher) restartSgtdi(opts Options) *sgtdiRun {
	delay := rootRetryMin
	for {
		select {
		case <-w.done:
			return nil
		case <-time.After(delay):
		}
		if rootIsDir(w.root) {
			run, err := startSgtdi(w.root, opts)
			if err == nil {
				return run
			}
			slog.Warn("fswatch: restarting the watcher after its root returned", "root", w.root, "err", err)
		}
		delay = min(2*delay, rootRetryMax)
	}
}

func rootIsDir(root string) bool {
	fi, err := os.Stat(root)
	return err == nil && fi.IsDir()
}

// pumpSgtdi forwards one run's events until the Watcher is closed, the root is
// lost, or the run stops on its own. An overflow marker, or anything on sgtdi's
// Dropped channel (its own buffer filled), means events were lost. A directory
// that appears is expanded (expandIfDir): inotify never reports files created
// in it before sgtdi has registered its watch.
func pumpSgtdi(w *Watcher, evs, dropped <-chan fswatcher.WatchEvent, stopped <-chan struct{}) runEnd {
	for {
		select {
		case <-w.done:
			return endClosed
		case <-stopped:
			if rootIsDir(w.root) {
				return endFailed
			}
			return endRootLost
		case ev, ok := <-evs:
			if !ok {
				return endFailed
			}
			if slices.Contains(ev.Types, fswatcher.EventOverflow) {
				w.signalLost()
				continue
			}
			op := opFromTypes(ev.Types)
			if op == 0 {
				continue
			}
			if filepath.Clean(ev.Path) == w.root {
				// Removed or renamed away, even if another directory is already
				// in its place: the inotify watch is on the old one.
				if op.Has(Remove | Rename) {
					return endRootLost
				}
				continue // the root's own metadata: nothing for a consumer to do
			}
			w.deliver(Event{Path: ev.Path, Op: op})
			w.expandIfDir(ev.Path, op, func(child string) { w.deliver(Event{Path: child, Op: Create}) })
		case _, ok := <-dropped:
			if !ok {
				return endFailed
			}
			w.signalLost()
		}
	}
}

// opFromTypes maps sgtdi's event types onto Op. EventUnknown still reports a
// change: something happened to the path, so the consumer must look.
func opFromTypes(types []fswatcher.EventType) Op {
	var op Op
	for _, t := range types {
		switch t {
		case fswatcher.EventCreate:
			op |= Create
		case fswatcher.EventMod:
			op |= Write
		case fswatcher.EventRemove:
			op |= Remove
		case fswatcher.EventRename:
			op |= Rename
		case fswatcher.EventChmod:
			op |= Chmod
		case fswatcher.EventUnknown:
			op |= Write
		}
	}
	return op
}
