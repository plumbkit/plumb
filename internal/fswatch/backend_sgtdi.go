//go:build linux || windows

package fswatch

import (
	"context"
	"errors"
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
	ready   chan struct{} // closed by sgtdi once every initial watch is in place
	stopped chan struct{} // closed when Watch returns
	err     error         // Watch's result; read only after stopped is closed
}

// startFunc starts one run over root; tests stand in for sgtdi with it.
type startFunc func(root string, opts Options) (*sgtdiRun, error)

// startSgtdi starts sgtdi/fswatcher over root: inotify on Linux,
// ReadDirectoryChangesW on Windows. Neither opens the files it watches. sgtdi
// debounces and applies the exclusion itself, exactly as plumb configured it
// before this package existed.
func startSgtdi(root string, opts Options) (*sgtdiRun, error) {
	ready := make(chan struct{})
	cfg := []fswatcher.WatcherOpt{
		fswatcher.WithSeverity(fswatcher.SeverityNone), // plumb does its own logging
		fswatcher.WithCooldown(opts.Cooldown),
		fswatcher.WithReadyChannel(ready),
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
	r := &sgtdiRun{src: src, cancel: cancel, ready: ready, stopped: make(chan struct{})}
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

// awaitReady waits until sgtdi has every initial watch in place: it walks the
// tree adding them after Watch starts, and a change made before a directory's
// watch exists is never reported. It reports false, with the run closed, when
// the run stopped first or done closed.
func (r *sgtdiRun) awaitReady(done <-chan struct{}) bool {
	select {
	case <-r.ready:
		return true
	case <-r.stopped:
	case <-done:
	}
	r.close()
	return false
}

// failure is why a run stopped, for the log.
func (r *sgtdiRun) failure() error {
	if r.err != nil {
		return r.err
	}
	return errors.New("the watcher stopped on its own")
}

func (w *Watcher) startBackend(opts Options) error {
	return w.startSupervised(opts, startSgtdi)
}

// startSupervised returns once the first run's watches are in place, so a
// consumer's first full scan, made after New returns, cannot miss a change
// made while sgtdi was still adding them.
func (w *Watcher) startSupervised(opts Options, start startFunc) error {
	run, err := start(w.root, opts)
	if err != nil {
		return err
	}
	if !run.awaitReady(w.done) {
		return fmt.Errorf("fswatch: watching %s: %w", w.root, run.failure())
	}
	w.wg.Go(func() { w.superviseSgtdi(opts, run, start) })
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
// a fresh run, and once that run's watches are all in place asks for another
// reconcile, which covers whatever was made in the root while it was unwatched.
// Asked any earlier, a consumer could finish reconciling before a watch existed
// and miss a change made in between for good.
//
// A run that stops on its own with the root still in place is not restarted:
// it would most likely fail the same way (an inotify watch limit, say) and turn
// into a reconcile loop. The Watcher is degraded for good instead.
func (w *Watcher) superviseSgtdi(opts Options, run *sgtdiRun, start startFunc) {
	for {
		end := pumpSgtdi(w, run.src.Events(), run.src.Dropped(), run.stopped)
		run.close()
		switch end {
		case endClosed:
			return
		case endFailed:
			w.degrade(run.failure())
			return
		}
		w.signalLost()
		next, err := w.restartSgtdi(opts, start)
		if err != nil {
			w.degrade(err)
			return
		}
		if next == nil {
			return // closed while waiting
		}
		run = next
		w.signalLost()
	}
}

// degrade reports a backend that will not run again: on Failed, so a consumer
// can fall back to periodic reconciliation, and on Lost for the changes already
// missed. Only the supervisor calls it, once, as it returns.
func (w *Watcher) degrade(err error) {
	slog.Warn("fswatch: watcher stopped; changes will not be seen until it is restarted", "root", w.root, "err", err)
	close(w.failed)
	w.signalLost()
}

// restartSgtdi waits for the root to be a directory again and starts a new run,
// backing off between attempts, and returns it once its watches are in place.
// It returns nil, nil once the Watcher is closed, and an error when a new run
// stops before it is ready while the root is still a directory: that is a
// failure, not another lost root.
func (w *Watcher) restartSgtdi(opts Options, start startFunc) (*sgtdiRun, error) {
	delay := rootRetryMin
	for {
		select {
		case <-w.done:
			return nil, nil
		case <-time.After(delay):
		}
		delay = min(2*delay, rootRetryMax)
		if !rootIsDir(w.root) {
			continue
		}
		run, err := start(w.root, opts)
		if err != nil {
			slog.Warn("fswatch: restarting the watcher after its root returned", "root", w.root, "err", err)
			continue
		}
		if run.awaitReady(w.done) {
			return run, nil
		}
		select {
		case <-w.done:
			return nil, nil
		default:
		}
		if rootIsDir(w.root) {
			return nil, run.failure()
		}
		// The root went again before the run was ready: wait for it once more.
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
