package topology

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sgtdi/fswatcher"
)

// watchCooldown debounces rapid successive events for the same path inside the
// OS watcher before they reach the indexer queue. A burst of writes to one file
// (an editor's incremental save, a formatter rewrite) collapses to one event.
const watchCooldown = 200 * time.Millisecond

// watchExcludeRegex keeps the OS watcher from descending into directories that
// are never indexed — primarily to avoid watching `.plumb/` (where the index's
// own topology.db/-wal/-shm live: watching them would feed every index write
// back as a change event, a self-trigger loop) and the usual heavy/irrelevant
// trees. It mirrors shouldSkipDir; shouldSkipPath is the authoritative guard
// applied before every enqueue, so a regex miss only costs a filtered event,
// never a wrong index.
const watchExcludeRegex = `(^|/)(\.[^/]+|vendor|node_modules|testdata|dist|build|__pycache__)(/|$)`

// indexSink is the slice of *Store the watcher drives. Defining it as an
// interface lets fsWatcher.handle be unit-tested against a fake without a real
// OS watcher.
type indexSink interface {
	// Enqueue schedules an incremental re-index of one path.
	Enqueue(path string)
	// Resync schedules a full workspace reconcile, used when individual events
	// were lost (OS queue overflow / dropped).
	Resync()
}

// fsWatcher bridges OS file-system events to the indexer. Any change to a file
// under the workspace — by this agent, another agent, or an external editor —
// enqueues an incremental re-index at the moment it happens, replacing
// time-based polling. Lost events (overflow/dropped) escalate to a full resync,
// so the index can never silently drift even though there is no periodic poll.
//
// Concurrency: Start launches two goroutines (the fswatcher pump and the event
// consumer); Stop signals them and joins. All sink calls happen on the consumer
// goroutine. Safe to Stop once; a second Stop is a no-op.
type fsWatcher struct {
	workspace string
	sink      indexSink
	w         fswatcher.Watcher

	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// newFSWatcher constructs a watcher rooted at workspace. It does not start
// watching until Start is called. An error here (e.g. the platform watcher
// cannot be created) lets the caller fall back to periodic resync.
func newFSWatcher(workspace string, sink indexSink) (*fsWatcher, error) {
	w, err := fswatcher.New(
		fswatcher.WithSeverity(fswatcher.SeverityNone), // plumb does its own logging
		fswatcher.WithCooldown(watchCooldown),
		fswatcher.WithExcRegex(watchExcludeRegex),
		fswatcher.WithPath(workspace, fswatcher.WithDepth(fswatcher.WatchNested)),
	)
	if err != nil {
		return nil, err
	}
	return &fsWatcher{workspace: workspace, sink: sink, w: w, done: make(chan struct{})}, nil
}

// Start launches the watcher pump and the event consumer.
func (fw *fsWatcher) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	fw.cancel = cancel
	fw.wg.Go(func() { _ = fw.w.Watch(ctx) })
	fw.wg.Go(fw.consume)
	slog.Info("topology: file watcher started", "workspace", fw.workspace)
}

// Stop ends watching and joins the goroutines. Idempotent.
func (fw *fsWatcher) Stop() {
	fw.stopOnce.Do(func() {
		close(fw.done)
		if fw.cancel != nil {
			fw.cancel()
		}
		fw.w.Close()
	})
	fw.wg.Wait()
}

// consume drains the watcher's event and dropped channels until Stop. A dropped
// event means the OS queue overflowed and individual changes were lost, so the
// whole tree is reconciled.
func (fw *fsWatcher) consume() {
	events := fw.w.Events()
	dropped := fw.w.Dropped()
	for {
		select {
		case <-fw.done:
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			fw.handle(ev)
		case _, ok := <-dropped:
			if !ok {
				return
			}
			fw.sink.Resync()
		}
	}
}

// handle maps one watch event to an indexer action: an overflow escalates to a
// full resync; an excluded path is ignored; anything else enqueues an
// incremental re-index of that path (Enqueue routes a now-missing file to a
// delete, so removals and renames need no special case).
func (fw *fsWatcher) handle(ev fswatcher.WatchEvent) {
	if watchHasOverflow(ev) {
		fw.sink.Resync()
		return
	}
	rel, err := filepath.Rel(fw.workspace, ev.Path)
	if err != nil || shouldSkipPath(rel) {
		return
	}
	fw.sink.Enqueue(rel)
}

// watchHasOverflow reports whether the event carries an overflow marker (the OS
// event buffer filled and changes were dropped).
func watchHasOverflow(ev fswatcher.WatchEvent) bool {
	return slices.Contains(ev.Types, fswatcher.EventOverflow)
}

// shouldSkipPath reports whether a workspace-relative path lies under any
// directory the indexer never indexes (reusing shouldSkipDir's canonical set,
// including dot-prefixed dirs such as .plumb and .git). It is the authoritative
// filter applied before every enqueue.
func shouldSkipPath(rel string) bool {
	if rel == "" || rel == "." || strings.HasPrefix(rel, "..") {
		return true
	}
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		if part != "" && shouldSkipDir(part) {
			return true
		}
	}
	return false
}
