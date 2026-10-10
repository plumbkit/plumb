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

// readyWithin bounds how long a run's initial walk may take: the first one,
// in New, and each one after a lost root returns. It is generous (50,000
// directories take under a second), and exists so a walk that hangs, on a
// stalled network mount say, fails New, or degrades the Watcher, instead of
// hanging the consumer's start or leaving the Watcher silently blind.
const readyWithin = time.Minute

// limitMsg is what a Watcher degraded by the inotify watch limit logs.
const limitMsg = "fswatch: the inotify watch limit left part of the tree unwatched; changes there will not be seen (raise fs.inotify.max_user_watches)"

// errNotReady is a run whose initial walk did not finish within its bound.
var errNotReady = errors.New("the initial watches were not in place in time")

// sgtdiRun is one running sgtdi watcher.
type sgtdiRun struct {
	src     fswatcher.Watcher
	cancel  context.CancelFunc
	ready   chan struct{} // closed by sgtdi once its initial walk has added every watch it could
	stopped chan struct{} // closed when Watch returns
	err     error         // Watch's result; read only after stopped is closed
	// probe reports whether adding a watch on a directory fails at the inotify
	// watch limit (inotifyLimitReached). Nil never reports it.
	probe func(dir string) bool
}

// startFunc starts one run over root; tests stand in for sgtdi with it.
type startFunc func(root string, opts Options) (*sgtdiRun, error)

// startSgtdi starts sgtdi/fswatcher over root: inotify on Linux,
// ReadDirectoryChangesW on Windows. Neither opens the files it watches. sgtdi
// debounces and applies the exclusion itself, exactly as plumb configured it
// before this package existed.
//
// The run owns its event channels, so sgtdi never closes them. sgtdi v1.3.0
// closes channels it owns when Watch returns, but does not wait for its
// debounce goroutine, so a flush already under way can send on the closed
// channel and panic (the race detector caught it under
// TestWatcher_CyclesDoNotLeak). A channel nobody closes takes that late send
// into a buffer nobody reads; sgtdi's sends never block.
func startSgtdi(root string, opts Options) (*sgtdiRun, error) {
	ready := make(chan struct{})
	events := make(chan fswatcher.WatchEvent, fswatcher.DefaultBufferSize)
	dropped := make(chan fswatcher.WatchEvent, max(fswatcher.DefaultBufferSize/fswatcher.MaxDroppedBufferRatio, fswatcher.MinDroppedBuffer))
	cfg := []fswatcher.WatcherOpt{
		fswatcher.WithSeverity(fswatcher.SeverityNone), // plumb does its own logging
		fswatcher.WithCooldown(opts.Cooldown),
		fswatcher.WithCustomChannels(events, dropped),
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
	r := &sgtdiRun{src: src, cancel: cancel, ready: ready, stopped: make(chan struct{}), probe: inotifyLimitReached}
	go func() {
		r.err = src.Watch(ctx)
		close(r.stopped)
	}()
	return r, nil
}

// close stops the run and waits for its Watch to return.
func (r *sgtdiRun) close() {
	r.abandon()
	<-r.stopped
}

// abandon stops the run without waiting for it. sgtdi cannot interrupt its
// initial walk, so waiting for a walk that hangs would hold the caller, Close
// included, for as long as the mount stalls. The run's goroutine returns
// whenever the walk does.
func (r *sgtdiRun) abandon() {
	r.cancel()
	r.src.Close()
}

// stopGrace bounds how long stop waits for a run to finish. A normal stop
// takes milliseconds, and waiting for it means a watcher started right after
// on the same tree does not overlap the old one's inotify watches; near the
// watch limit the overlap could leave the new walk partial, and the new
// Watcher degraded for good. A walk sgtdi cannot interrupt is left to end on
// its own once the grace runs out.
const stopGrace = 2 * time.Second

// stop stops the run and waits for it to finish, but for at most stopGrace.
func (r *sgtdiRun) stop() {
	r.abandon()
	grace := time.NewTimer(stopGrace)
	defer grace.Stop()
	select {
	case <-r.stopped:
	case <-grace.C:
	}
}

// awaitReady waits, for at most within, until sgtdi's initial walk has added
// its watches: the walk runs after Watch starts, and a change made before a
// directory's watch exists is never reported. It reports true once they are
// in place. Otherwise the run is stopped: closed when it stopped on its own,
// stopped (with a bounded wait) when done closed, and abandoned when within
// passed, with errNotReady: that walk is hung. It never waits for readiness
// after done.
func (r *sgtdiRun) awaitReady(done <-chan struct{}, within time.Duration) (bool, error) {
	timeout := time.NewTimer(within)
	defer timeout.Stop()
	select {
	case <-r.ready:
		return true, nil
	case <-r.stopped:
		r.close()
		return false, nil
	case <-done:
		r.stop()
		return false, nil
	case <-timeout.C:
		r.abandon()
		return false, errNotReady
	}
}

// partial reports whether sgtdi left directories unwatched. It counts a
// subdirectory it could not watch at the inotify watch limit, skips it and its
// subtree, and carries on: in the initial walk, and in a directory that
// arrives later.
func (r *sgtdiRun) partial() bool {
	return r.src.Stats().EventsPartial > 0
}

// limitReached reports whether a directory that just arrived may have been
// left unwatched at the inotify watch limit. sgtdi adds the watches for a new
// directory before it reports the directory's event. It counts the
// subdirectories it could not watch (partial), but it discards the error for
// the directory itself, so the probe tries a watch of its own. ENOSPC there
// means the limit is exhausted, and sgtdi's attempt a moment earlier almost
// certainly failed too. When sgtdi's own watch took the last slot, the probe
// reports the limit as well; degrading a Watcher that is, for now, complete is
// the safe side to err on, since the next directory would go unwatched.
//
// It is best effort. A watch freed between sgtdi's attempt and the probe (a
// directory deleted in that moment) hides the limit, and a directory whose
// event never arrives (sgtdi's buffers full: Lost, not Failed) is never
// probed.
func (r *sgtdiRun) limitReached(dir string) bool {
	return r.partial() || (r.probe != nil && r.probe(dir))
}

// failure is why a run stopped, for the log.
func (r *sgtdiRun) failure() error {
	if r.err != nil {
		return r.err
	}
	return errors.New("the watcher stopped on its own")
}

func (w *Watcher) startBackend(opts Options) error {
	return w.startSupervised(opts, startSgtdi, readyWithin)
}

// startSupervised returns once the first run's initial walk is done, so a
// consumer's first full scan, made after New returns, cannot miss a change
// made while sgtdi was still adding watches. A walk that does not finish
// within the given bound fails New; the run is abandoned, not waited for.
func (w *Watcher) startSupervised(opts Options, start startFunc, within time.Duration) error {
	run, err := start(w.root, opts)
	if err != nil {
		return err
	}
	ready, err := run.awaitReady(nil, within)
	if err != nil {
		return fmt.Errorf("fswatch: watching %s: the initial watches were not in place within %v", w.root, within)
	}
	if !ready {
		return fmt.Errorf("fswatch: watching %s: %w", w.root, run.failure())
	}
	w.wg.Go(func() { w.superviseSgtdi(opts, run, start, within) })
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
// a fresh run, and once that run's initial walk is done asks for another
// reconcile, which covers whatever was made in the root while it was unwatched.
// Asked any earlier, a consumer could finish reconciling before a watch existed
// and miss a change made in between for good.
//
// A run that stops on its own with the root still in place is not restarted:
// it would most likely fail the same way (the root itself at an inotify watch
// limit, say) and turn into a reconcile loop. The Watcher is degraded for good
// instead. So is one whose restart walk does not finish within the bound. So,
// too, is a run that left directories unwatched at the inotify watch limit, in
// its walk or in a directory that arrived later, though it keeps delivering
// what it does watch.
func (w *Watcher) superviseSgtdi(opts Options, run *sgtdiRun, start startFunc, within time.Duration) {
	for {
		if run.partial() {
			w.degrade(limitMsg, nil)
		}
		end := pumpSgtdi(w, run.src.Events(), run.src.Dropped(), run.stopped, run.limitReached)
		if end == endFailed {
			run.close() // it has stopped already, so this is immediate, and run.err is safe to read
		} else {
			run.stop() // waits a bounded time: never on a walk sgtdi cannot interrupt (a new directory on a stalled mount)
		}
		switch end {
		case endClosed:
			return
		case endFailed:
			w.degrade("fswatch: watcher stopped; changes will not be seen until it is restarted", run.failure())
			return
		}
		w.signalLost()
		next, err := w.restartSgtdi(opts, start, within)
		if err != nil {
			w.degrade("fswatch: watcher stopped; changes will not be seen until it is restarted", err)
			return
		}
		if next == nil {
			return // closed while waiting
		}
		run = next
		w.signalLost()
	}
}

// degrade reports a Watcher that can no longer see every change: on Failed, so
// a consumer can fall back to periodic reconciliation, and on Lost for what was
// already missed. Failed is closed once; only the supervisor calls degrade, so
// the check before closing cannot race.
func (w *Watcher) degrade(msg string, err error) {
	slog.Warn(msg, "root", w.root, "err", err)
	if !w.isFailed() {
		close(w.failed)
	}
	w.signalLost()
}

// isFailed reports whether Failed is closed.
func (w *Watcher) isFailed() bool {
	select {
	case <-w.failed:
		return true
	default:
		return false
	}
}

// restartSgtdi waits for the root to be a directory again and starts a new run,
// backing off between attempts, and returns it once its watches are in place.
// It returns nil, nil once the Watcher is closed. It returns an error when a
// new run stops before it is ready while the root is still a directory (a
// failure, not another lost root), or when its walk is not done within the
// bound (a hung walk, abandoned rather than waited for).
func (w *Watcher) restartSgtdi(opts Options, start startFunc, within time.Duration) (*sgtdiRun, error) {
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
		ready, err := run.awaitReady(w.done, within)
		if ready {
			return run, nil
		}
		select {
		case <-w.done:
			return nil, nil
		default:
		}
		if err != nil {
			return nil, fmt.Errorf("restarting after the root returned: %w (within %v)", err, within)
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
// that appears is expanded (see expandIfDir): inotify never reports files
// created in it before sgtdi has registered its watch. It is also checked
// against the inotify watch limit (limitReached), until the Watcher is
// degraded; after that there is nothing left to report.
func pumpSgtdi(w *Watcher, evs, dropped <-chan fswatcher.WatchEvent, stopped <-chan struct{}, limitReached func(dir string) bool) runEnd {
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
			if w.pumpEvent(ev, limitReached) {
				return endRootLost
			}
		case _, ok := <-dropped:
			if !ok {
				return endFailed
			}
			w.signalLost()
		}
	}
}

// pumpEvent handles one sgtdi event for pumpSgtdi, and reports whether it says
// the root itself was removed or renamed away.
func (w *Watcher) pumpEvent(ev fswatcher.WatchEvent, limitReached func(dir string) bool) (rootLost bool) {
	if slices.Contains(ev.Types, fswatcher.EventOverflow) {
		w.signalLost()
		return false
	}
	op := opFromTypes(ev.Types)
	if op == 0 {
		return false
	}
	if filepath.Clean(ev.Path) == w.root {
		// Removed or renamed away, even if another directory is already in its
		// place: the inotify watch is on the old one. Anything else is the
		// root's own metadata, with nothing for a consumer to do.
		return op.Has(Remove | Rename)
	}
	w.deliver(Event{Path: ev.Path, Op: op})
	if arrivedDir(ev.Path, op) {
		w.expand(ev.Path, func(child string) { w.deliver(Event{Path: child, Op: Create}) })
		if !w.isFailed() && limitReached(ev.Path) {
			w.degrade(limitMsg, nil)
		}
	}
	return false
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
