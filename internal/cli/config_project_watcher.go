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
// from that goroutine and must not be called holding mu.

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
	// ctx parents every watch goroutine, so a daemon shutdown stops them all.
	ctx      context.Context
	dispatch func(workspace string)
	debounce time.Duration

	mu      sync.Mutex
	watches map[string]*projectConfigWatch
}

// newProjectConfigWatchManager builds the manager. dispatch is called once
// per debounced change with the canonical workspace root — in production it
// is connRegistry.reloadProject; tests substitute a signalling closure.
func newProjectConfigWatchManager(ctx context.Context, dispatch func(workspace string)) *projectConfigWatchManager {
	return &projectConfigWatchManager{
		ctx:      ctx,
		dispatch: dispatch,
		debounce: projectConfigDebounce,
		watches:  make(map[string]*projectConfigWatch),
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
			go m.run(ctx, w, root, ready)
		}
		m.mu.Unlock()
		<-ready
		return
	}
	ctx, cancel := context.WithCancel(m.ctx)
	w := &projectConfigWatch{refs: 1, cancel: cancel, ready: make(chan struct{})}
	m.watches[root] = w
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

// close stops every watcher. Called on daemon shutdown; the parent ctx does
// the same, so this exists for tests and orderly teardown.
func (m *projectConfigWatchManager) close() {
	m.mu.Lock()
	for root, w := range m.watches {
		w.cancel()
		delete(m.watches, root)
	}
	m.mu.Unlock()
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
// covers the workspace. Runtime errors mark the watch failed (the fallback
// poll re-engages) but the loop keeps running: later events still dispatch,
// and an fsnotify error is often a dropped-event notice rather than a dead
// watcher.
func (m *projectConfigWatchManager) run(ctx context.Context, w *projectConfigWatch, root string, ready chan struct{}) {
	// dead lets acquire distinguish a loop that EXITED (safe to retry) from
	// one that merely saw a transient error and is still running.
	defer w.dead.Store(true)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		w.failed.Store(true)
		close(ready)
		slog.Warn("daemon: project config watcher unavailable — 30s poll fallback active", "workspace", root, "err", err)
		return
	}
	defer watcher.Close()

	// The root always exists for a pinned workspace; .plumb may not. Watching
	// the root (non-recursive) catches the .plumb dir itself being created,
	// renamed or removed — the case where a project gains (or loses) its whole
	// config after sessions attached.
	if err := watcher.Add(root); err != nil {
		w.failed.Store(true)
		close(ready)
		slog.Warn("daemon: project config watcher unavailable — 30s poll fallback active", "workspace", root, "err", err)
		return
	}
	plumbDir := filepath.Join(root, ".plumb")
	plumbWatched := watchPlumbDir(watcher, plumbDir, false)
	close(ready)
	slog.Debug("daemon: watching project config for changes", "workspace", root)

	timer := time.NewTimer(m.debounce)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if filepath.Clean(event.Name) == plumbDir {
				// The .plumb entry itself changed. A remove/rename of the
				// directory kills the OS watch on the old inode, so drop the
				// latch first — otherwise every later config.toml edit stays
				// silently invisible (and failed is never set, so the poll
				// fallback never engages either). Then re-arm — a no-op while
				// the dir is still the watched one — and reload: removing
				// .plumb revokes what its config granted.
				if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
					plumbWatched = false
				}
				plumbWatched = watchPlumbDir(watcher, plumbDir, plumbWatched)
				rearmProjectTimer(timer, m.debounce)
				continue
			}
			if projectConfigEvent(event.Name, plumbDir, event.Op) {
				rearmProjectTimer(timer, m.debounce)
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			w.failed.Store(true)
			slog.Warn("daemon: project config watcher error — 30s poll fallback active", "workspace", root, "err", err)
		case <-timer.C:
			m.dispatch(root)
		}
	}
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
