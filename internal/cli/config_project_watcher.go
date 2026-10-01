package cli

// config_project_watcher.go — the daemon-owned, per-workspace project-config
// watcher (PLAN-414).
//
// Correctness mechanism for project-config hot reload. Before it, each
// connection polled its own <workspace>/.plumb/config.toml every 30 s
// (startConfigWatcher), and a live incident showed two attached sessions
// sitting stale for minutes after a trusted project file changed. Now one
// fsnotify watcher per live workspace watches for config.toml changes and
// dispatches through connRegistry.reloadProject, so EVERY session pinned to
// the workspace re-applies exactly once per change — without a reconnect, a
// daemon restart, or the global value moving.
//
// The 30 s poll remains as a bounded fallback, engaged only when the watcher
// for a workspace is absent (manager not wired — tests) or has failed (see
// connSession.reconcileProjectConfig); a failed watch is observable in the
// daemon log and via healthy().
//
// What is watched: the workspace ROOT (always present for a pinned session)
// plus <root>/.plumb when it exists. Watching the root is what makes a .plumb
// directory created AFTER attach visible, and watching .plumb directly is
// what survives the inode swaps of atomic editor saves on config.toml. Events
// are filtered to the config file (and the .plumb dir entry itself, whose
// create/remove swaps what the config resolves to) and coalesced over a short
// debounce window, matching the global watcher's contract.
//
// Self-trigger safety: dispatch only re-reads the file; no apply path writes
// the project config back, so a reload never produces a new event.
//
// Concurrency: all methods are safe for concurrent use. Each workspace runs
// one goroutine for the watch's lifetime; the dispatch callback is invoked
// from that goroutine and must not be called holding mu. close waits — for at
// most projectWatchCloseGrace — for every goroutine, including ones a release
// cancelled, to stop and close its OS watcher; release does not wait, because
// it can run on a watch goroutine's own dispatch path (a reload that moves a
// session's pin).

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/plumbkit/plumb/internal/paths"
)

// projectConfigDebounce collapses the event burst a single save emits into
// one dispatch. Same window as the global config watcher.
const projectConfigDebounce = 250 * time.Millisecond

// projectWatchCloseGrace bounds how long close waits for the watch goroutines
// to finish. A healthy goroutine stops and closes its watcher within
// microseconds of its cancel; the bound exists for one that cannot, because it
// is inside dispatch (connRegistry.reloadProject), which can queue on a
// session's mutation lane while that lane is held across slow work. close runs
// on the orderly daemon shutdown path, which must stay under shutdownHardDeadline
// (TestShutdownHardDeadlineExceedsInnerGraces sums this grace in), so a wedged
// dispatch is abandoned and logged instead of holding the daemon until the
// watchdog forces the exit. Tests that need the full wait raise closeGrace.
const projectWatchCloseGrace = 500 * time.Millisecond

// projectConfigWatch is one workspace's registration: the refcount of live
// connections pinned to it, the cancel that stops its goroutine, and two
// latches. failed records that the OS watcher could not start or errored at
// runtime (the per-session poll owns that workspace until a retry); dead
// records that the run goroutine has EXITED. acquire retries only a watch
// that is both failed and dead: a live loop that saw a transient fsnotify
// error keeps its goroutine — respawning over it would orphan the old
// goroutine's cancel and double-dispatch every change. ready is closed by
// the watch goroutine once the OS watcher is attached (or has failed):
// acquire waits on it, so a config write can never land in the window
// between a session pinning the workspace and the watcher being live.
type projectConfigWatch struct {
	refs   int
	cancel context.CancelFunc
	failed atomic.Bool
	dead   atomic.Bool
	ready  chan struct{}
}

// projectConfigWatchManager owns the live per-workspace watchers, keyed by
// canonical workspace root so symlink and trailing-slash aliases share one
// registration. Not nil-safe: callers hold a concrete manager or none.
type projectConfigWatchManager struct {
	// ctx parents every watch goroutine, so a daemon shutdown — or close,
	// through stop — ends them all, even one whose own cancel was lost.
	ctx      context.Context
	stop     context.CancelFunc
	dispatch func(workspace string)
	debounce time.Duration
	// newWatcher builds each OS watcher: fsnotify.NewWatcher, or a test's
	// wrapper that captures each watcher it builds.
	newWatcher func() (*fsnotify.Watcher, error)
	// recreateInterval is watcherRecreateInterval; tests widen it so "lost
	// again straight away" does not depend on scheduling.
	recreateInterval time.Duration
	// closeGrace is projectWatchCloseGrace: how long close waits for the watch
	// goroutines before abandoning them. Tests set it to wait out, or to bound
	// tightly, a goroutine they hold open.
	closeGrace time.Duration
	// testErrs is a test seam: an error sent on it reaches a run loop exactly
	// as if its OS watcher had reported it. Nil in production, and a nil
	// channel never fires in a select. (Tests must not send on fsnotify's own
	// Errors channel: its reader closes that channel.)
	testErrs chan error

	mu      sync.Mutex
	watches map[string]*projectConfigWatch
	// closed stops acquire starting goroutines once close has begun, so wg.Add
	// can never race wg.Wait.
	closed bool
	// wg counts live run goroutines, released-but-still-closing ones included.
	wg sync.WaitGroup
}

// newProjectConfigWatchManager builds the manager. dispatch is called once
// per debounced change with the canonical workspace root — in production it
// is connRegistry.reloadProject; tests substitute a signalling closure.
func newProjectConfigWatchManager(ctx context.Context, dispatch func(workspace string)) *projectConfigWatchManager {
	ctx, stop := context.WithCancel(ctx)
	return &projectConfigWatchManager{
		ctx:        ctx,
		stop:       stop,
		dispatch:   dispatch,
		debounce:   projectConfigDebounce,
		newWatcher: fsnotify.NewWatcher,

		recreateInterval: watcherRecreateInterval,
		closeGrace:       projectWatchCloseGrace,
		watches:          make(map[string]*projectConfigWatch),
	}
}

// acquire registers a live connection on workspace's watcher, starting the
// watch goroutine on the first reference. The root is canonicalised first so
// a workspace reachable by two spellings shares one watcher. An empty or
// unresolvable workspace is a no-op.
func (m *projectConfigWatchManager) acquire(workspace string) {
	root := paths.Canonical(workspace)
	if root == "" {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	if w, ok := m.watches[root]; ok {
		w.refs++
		ready := w.ready
		// A watch whose goroutine DIED (watcher creation/attach failed) gets a
		// fresh attempt here: the poll fallback covered the gap, and a new
		// attachment is the natural retry point. A watch that failed but is
		// still running is left alone — the poll covers it, and a respawn
		// would orphan the live goroutine's cancel and double-dispatch.
		if w.failed.Load() && w.dead.Load() {
			w.failed.Store(false)
			w.dead.Store(false)
			ready = make(chan struct{})
			w.ready = ready
			ctx, cancel := context.WithCancel(m.ctx)
			w.cancel = cancel
			m.wg.Add(1)
			go m.run(ctx, w, root, ready)
		}
		m.mu.Unlock()
		<-ready
		return
	}
	ctx, cancel := context.WithCancel(m.ctx)
	w := &projectConfigWatch{refs: 1, cancel: cancel, ready: make(chan struct{})}
	m.watches[root] = w
	m.wg.Add(1)
	m.mu.Unlock()
	go m.run(ctx, w, root, w.ready)
	<-w.ready
}

// release drops one connection's reference, stopping and removing the watcher
// when the last session leaves the workspace. Unknown workspaces are a no-op.
func (m *projectConfigWatchManager) release(workspace string) {
	root := paths.Canonical(workspace)
	m.mu.Lock()
	w, ok := m.watches[root]
	if ok {
		w.refs--
		if w.refs <= 0 {
			delete(m.watches, root)
			w.cancel()
		}
	}
	m.mu.Unlock()
}

// healthy reports whether workspace currently has an active watcher that has
// not failed. The per-session poll consults this: healthy means the watcher
// owns correctness and the poll skips; anything else re-engages the fallback.
func (m *projectConfigWatchManager) healthy(workspace string) bool {
	root := paths.Canonical(workspace)
	m.mu.Lock()
	w, ok := m.watches[root]
	m.mu.Unlock()
	return ok && !w.failed.Load()
}

// close stops every watcher and waits until each watch goroutine has stopped
// and closed its OS watcher (closeFSWatcher), but for no longer than
// closeGrace: a goroutine stuck in dispatch is abandoned and logged, so the
// orderly shutdown stays bounded. Called on daemon shutdown; tests call it
// before removing the directories they watched, because fsnotify's kqueue
// backend can close a descriptor twice when Close races a delete inside the
// watched tree. The wait narrows that window rather than closing it: each
// reader's final descriptor closes can trail it by microseconds (see
// closeFSWatcher). Acquire is a no-op afterwards.
func (m *projectConfigWatchManager) close() {
	m.mu.Lock()
	m.closed = true
	for root, w := range m.watches {
		w.cancel()
		delete(m.watches, root)
	}
	m.stop()
	m.mu.Unlock()
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	waitWithTimeout(done, m.closeGrace, "project config watchers")
}

// refs reports the live-connection refcount on workspace's watcher (test seam).
func (m *projectConfigWatchManager) refs(workspace string) int {
	root := paths.Canonical(workspace)
	m.mu.Lock()
	defer m.mu.Unlock()
	if w, ok := m.watches[root]; ok {
		return w.refs
	}
	return 0
}

// run is the per-workspace watch loop: attach, filter, debounce, dispatch.
// It closes ready exactly once — after the watcher is attached, or after
// marking the watch failed — so acquire never returns before the OS watch is
// live. A watcher that cannot be created or attached marks the watch failed
// and returns: the daemon keeps running and the per-session poll fallback
// covers the workspace. Runtime errors are projectWatchLoop.onError's call.
func (m *projectConfigWatchManager) run(ctx context.Context, w *projectConfigWatch, root string, ready chan struct{}) {
	// Deferred first so it runs last: close waits on wg, so it also waits for
	// the OS watcher below to be closed.
	defer m.wg.Done()
	// dead lets acquire distinguish a loop that EXITED (safe to retry) from
	// one that merely saw a transient error and is still running.
	defer w.dead.Store(true)
	l := &projectWatchLoop{
		root:       root,
		plumbDir:   filepath.Join(root, ".plumb"),
		debounce:   m.debounce,
		newWatcher: m.newWatcher,

		recreateInterval: m.recreateInterval,
	}
	if err := l.open(); err != nil {
		w.failed.Store(true)
		close(ready)
		slog.Warn("daemon: project config watcher unavailable — 30s poll fallback active", "workspace", root, "err", err)
		return
	}
	defer l.close()
	close(ready)
	slog.Debug("daemon: watching project config for changes", "workspace", root)

	l.timer = time.NewTimer(m.debounce)
	if !l.timer.Stop() {
		<-l.timer.C
	}
	defer l.timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-l.watcher.Events:
			if !ok {
				return
			}
			l.onEvent(event)
		case err, ok := <-l.watcher.Errors:
			if !ok || !l.onError(w, err) {
				return
			}
		case err := <-m.testErrs:
			if !l.onError(w, err) {
				return
			}
		case <-l.timer.C:
			m.dispatch(root)
		}
	}
}

// projectWatchLoop is one run goroutine's OS-watcher state. It is owned by
// that goroutine alone and never shared, so it needs no locking.
type projectWatchLoop struct {
	root, plumbDir string
	debounce       time.Duration
	newWatcher     func() (*fsnotify.Watcher, error)

	recreateInterval time.Duration

	watcher      *fsnotify.Watcher
	plumbWatched bool
	timer        *time.Timer
	recreatedAt  time.Time
}

// open creates the OS watcher and attaches it. The root always exists for a
// pinned workspace; .plumb may not. Watching the root (non-recursive) catches
// the .plumb dir itself being created, renamed or removed — the case where a
// project gains (or loses) its whole config after sessions attached.
//
// An attach that fails because a descriptor was closed underneath it
// (fsWatcherLost) says nothing about the workspace, so it gets one immediate
// second attempt on a fresh watcher before the watch is declared failed.
func (l *projectWatchLoop) open() error {
	err := l.openOnce()
	if fsWatcherLost(err) {
		err = l.openOnce()
	}
	return err
}

func (l *projectWatchLoop) openOnce() error {
	watcher, err := l.newWatcher()
	if err != nil {
		return err
	}
	if err := watcher.Add(l.root); err != nil {
		closeFSWatcher(watcher)
		return err
	}
	l.watcher = watcher
	l.plumbWatched = watchPlumbDir(watcher, l.plumbDir, false)
	return nil
}

// close closes the current OS watcher, if any, and waits for its reader to
// stop delivering (see closeFSWatcher).
func (l *projectWatchLoop) close() {
	closeFSWatcher(l.watcher)
	l.watcher = nil
}

// onEvent filters one fsnotify event and re-arms the debounce timer for a
// reload-worthy one.
func (l *projectWatchLoop) onEvent(event fsnotify.Event) {
	if filepath.Clean(event.Name) == l.plumbDir {
		// The .plumb entry itself changed. A remove/rename of the directory
		// kills the OS watch on the old inode, so drop the latch first —
		// otherwise every later config.toml edit stays silently invisible (and
		// failed is never set, so the poll fallback never engages either).
		// Then re-arm — a no-op while the dir is still the watched one — and
		// reload: removing .plumb revokes what its config granted.
		if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
			l.plumbWatched = false
		}
		l.plumbWatched = watchPlumbDir(l.watcher, l.plumbDir, l.plumbWatched)
		rearmProjectTimer(l.timer, l.debounce)
		return
	}
	if projectConfigEvent(event.Name, l.plumbDir, event.Op) {
		rearmProjectTimer(l.timer, l.debounce)
	}
}

// onError handles one fsnotify error and reports whether the loop should keep
// running.
//
// A lost watcher (fsWatcherLost) can never deliver another event — its
// kqueue reader just spins on the same error — so it is closed and replaced
// in place, and a reload is scheduled to pick up whatever changed while it
// was blind. That turns a stolen descriptor into a sub-second blip instead of
// a workspace left to the 30 s poll. Any other error keeps the long-standing
// contract: mark the watch failed (the poll re-engages) and keep running,
// because an fsnotify error is often a dropped-event notice, not a dead
// watcher.
func (l *projectWatchLoop) onError(w *projectConfigWatch, err error) bool {
	if !fsWatcherLost(err) {
		w.failed.Store(true)
		slog.Warn("daemon: project config watcher error — 30s poll fallback active", "workspace", l.root, "err", err)
		return true
	}
	// Lost again this soon is not a one-off: leave the workspace to the poll
	// fallback and the next acquire's retry rather than spin recreating.
	if !l.recreatedAt.IsZero() && time.Since(l.recreatedAt) < l.recreateInterval {
		w.failed.Store(true)
		slog.Warn("daemon: project config watcher lost again straight after a recreate — 30s poll fallback active", "workspace", l.root, "err", err)
		return false
	}
	// Close the dead watcher BEFORE opening its replacement: descriptors are
	// allocated lowest-free-first, so a replacement opened first could be
	// handed the very number the dead watcher's reader closes on its way out.
	// Closing first only shrinks that window: the reader closes its kqueue and
	// pipe just after Events, so they can still trail the close by microseconds
	// (see closeFSWatcher).
	l.close()
	if rerr := l.open(); rerr != nil {
		w.failed.Store(true)
		slog.Warn("daemon: project config watcher lost and not recreated — 30s poll fallback active", "workspace", l.root, "err", err, "recreate_err", rerr)
		return false
	}
	l.recreatedAt = time.Now()
	w.failed.Store(false)
	slog.Warn("daemon: project config watcher lost its OS handle — recreated", "workspace", l.root, "err", err)
	rearmProjectTimer(l.timer, l.debounce)
	return true
}

// watchPlumbDir attaches the .plumb subdirectory watch unless the latch says
// it is already attached, returning the new latch state. A false result means
// the directory does not exist yet — the root watch will see it appear.
func watchPlumbDir(watcher *fsnotify.Watcher, plumbDir string, watched bool) bool {
	if watched {
		return true
	}
	return watcher.Add(plumbDir) == nil
}

// projectConfigEvent reports whether an event under .plumb refers to
// config.toml and is reload-worthy. Wider than the global watcher's
// shouldReload by one op: REMOVE. A deleted project config is a REVOCATION
// event — applyProjectConfig fails closed to the global policy — so it must
// dispatch like any write (deleting the file was previously a way to keep
// what it granted). The global watcher keeps its narrower set: a missing
// global file resolves to compiled defaults, which a reload cannot improve on.
func projectConfigEvent(name, plumbDir string, op fsnotify.Op) bool {
	if filepath.Dir(name) != plumbDir || filepath.Base(name) != "config.toml" {
		return false
	}
	return op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) != 0
}

// rearmProjectTimer restarts the debounce window without leaking a stale
// fire (the stop-drain-reset dance from the global watcher).
func rearmProjectTimer(timer *time.Timer, debounce time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(debounce)
}
