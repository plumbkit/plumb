package topology

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sgtdi/fswatcher"

	"github.com/plumbkit/plumb/internal/ignore"
)

// watchCooldown debounces rapid successive events for the same path inside the
// OS watcher before they reach the indexer queue. A burst of writes to one file
// (an editor's incremental save, a formatter rewrite) collapses to one event.
const watchCooldown = 200 * time.Millisecond

// watchExcludeRegexFor builds the OS watcher's source exclusion for one
// workspace. It keeps events for paths that are never indexed from reaching the
// consumer — primarily `.plumb/` (where the index's own topology.db/-wal/-shm
// live: those events would feed every index write back as a change, a
// self-trigger loop) and the usual heavy/irrelevant trees. It mirrors
// shouldSkipDir; shouldSkipPath is the authoritative guard applied before every
// enqueue, so a regex miss only costs a filtered event, never a wrong index.
//
// Two things about it are deliberate, and both were bugs in the fixed string it
// replaces (`(^|/)(\.[^/]+|vendor|…)(/|$)`).
//
// It is ANCHORED AT THE WORKSPACE ROOT. fswatcher tests the exclusion against
// the absolute event path, so an unanchored dot-component branch also matched
// the directories a workspace merely LIVES under: a checkout at ~/.config/app
// or under a dot-prefixed test cache had every one of its events excluded and
// the watcher delivered nothing at all.
//
// The dot branch requires a TRAILING SEPARATOR, so it excludes dot DIRECTORIES
// inside the tree and not a dot component at the end of a path. fswatcher
// matches an exclusion against both the full path and the base name
// (filters.go: patternFilter.ShouldInclude), so the old `(/|$)` anchor matched
// the base name ".gitignore" and the OS watcher NEVER delivered an event for an
// ignore file. Anything reacting to .gitignore edits would have passed a direct
// unit test of handle() and fired zero times in production — see
// TestFSWatcher_ThroughOSWatcher, which drives real filesystem events.
//
// Relaxing the dot branch cannot widen what is WATCHED: fswatcher applies the
// filter to events only, never to watch registration (inotify's recursive add
// walks every subdirectory regardless; FSEvents streams whole subtrees), so the
// cost is at most a few extra events that shouldSkipPath then drops — a dot FILE
// such as .env or .DS_Store. Every `/.git/…` and `/.plumb/…` event is still
// excluded at the source, so the self-trigger loop stays closed.
func watchExcludeRegexFor(workspace string) string {
	// Both separators, because the event path's separator is the platform's and
	// the quoted root carries whichever filepath.Clean produced.
	const sep = `[\\/]`
	root := regexp.QuoteMeta(filepath.Clean(workspace))
	return `^` + root + sep + `(?:.*` + sep + `)?(?:(?:vendor|node_modules|testdata|dist|build|__pycache__)(?:` + sep + `|$)|\.[^\\/]+` + sep + `)`
}

// watcherIgnoreCacheMax bounds the per-directory ignore-stack cache. Reached
// only by a workspace with more live directories than this; the cache is then
// dropped wholesale and rebuilt lazily, which costs a re-read of the ignore
// files on the path of the next event rather than unbounded memory.
const watcherIgnoreCacheMax = 4096

// isIgnoreFileName reports whether base is one of the two files whose CONTENT
// decides what the indexer skips. Kept in sync with ignore.Stack.Load.
func isIgnoreFileName(base string) bool {
	return base == ".gitignore" || base == ".ignore"
}

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

	// excludePatterns is the sanitised [topology] exclude_patterns list, applied
	// here for the same reason the resync walk applies it: without it a write to
	// an excluded file re-indexes it, and the row survives until the next resync
	// prunes it.
	excludePatterns []string

	// ignoreCache maps an absolute DIRECTORY to the ignore.Stack in force
	// inside it, built top-down from the workspace root.
	//
	// Why a cache and not a stack: shouldSkipPath answers from the relative path
	// alone, but .gitignore rules are per-directory, so deciding one event means
	// reading every ignore file from the root down to it. A watcher sees events
	// in arbitrary order and re-visits the same directories constantly, so the
	// work is worth remembering; a walker's stack is not available to it because
	// there is no walk. Invalidation is wholesale — any .gitignore/.ignore event
	// drops the whole map — because that event already forces a full resync, and
	// a rule added in one directory can change the verdict for its whole subtree.
	ignoreMu    sync.Mutex
	ignoreCache map[string]ignore.Stack

	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// newFSWatcher constructs a watcher rooted at workspace. It does not start
// watching until Start is called. An error here (e.g. the platform watcher
// cannot be created) lets the caller fall back to periodic resync.
func newFSWatcher(workspace string, sink indexSink, excludePatterns []string) (*fsWatcher, error) {
	w, err := fswatcher.New(
		fswatcher.WithSeverity(fswatcher.SeverityNone), // plumb does its own logging
		fswatcher.WithCooldown(watchCooldown),
		fswatcher.WithExcRegex(watchExcludeRegexFor(workspace)),
		fswatcher.WithPath(workspace, fswatcher.WithDepth(fswatcher.WatchNested)),
	)
	if err != nil {
		return nil, err
	}
	return &fsWatcher{
		workspace:       filepath.Clean(workspace),
		sink:            sink,
		w:               w,
		excludePatterns: excludePatterns,
		done:            make(chan struct{}),
	}, nil
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
// full resync; a change to an ignore file escalates to one too; an excluded
// path is dropped; anything else enqueues an incremental re-index of that path
// (Enqueue routes a now-missing file to a delete, so removals and renames need
// no special case).
func (fw *fsWatcher) handle(ev fswatcher.WatchEvent) {
	if watchHasOverflow(ev) {
		fw.sink.Resync()
		return
	}
	rel, err := filepath.Rel(fw.workspace, ev.Path)
	if err != nil {
		return
	}
	// Ahead of shouldSkipPath, which skips every dot-prefixed component and so
	// would drop .gitignore itself. One edit can both exclude files already
	// indexed and re-include files pruned earlier, and only a full resync
	// reconciles both directions — pruneDeleted is what un-indexes the newly
	// ignored ones.
	if isIgnoreFileName(filepath.Base(rel)) {
		if dir := filepath.Dir(rel); dir == "." || !shouldSkipPath(dir) {
			fw.dropIgnoreCache()
			fw.sink.Resync()
		}
		return
	}
	if shouldSkipPath(rel) {
		return
	}
	if matchesExcludePattern(fw.excludePatterns, rel) {
		return
	}
	if fw.ignoredPath(ev.Path) {
		return
	}
	fw.sink.Enqueue(rel)
}

// ignoredPath reports whether abs is excluded by a .gitignore / .ignore rule.
//
// It walks the ancestor chain from the workspace root down, testing each
// component with the stack in force at its parent, because ignore.Stack's
// contract is written for a top-down walk that PRUNES: asked directly about
// build/x/y.go with nothing pruned, IsIgnored answers false, since `build/` is
// directory-only and no rule matches the file itself. A watcher samples paths
// in arbitrary order, which is exactly the caller that contract tells to test
// its own ancestors.
//
// The final component's kind is resolved with Lstat because an event may be a
// directory create or rename as easily as a file write. A vanished path (the
// delete case) is treated as a file: if it was ignored it was never indexed, and
// if it was not, the enqueue below routes to a delete.
func (fw *fsWatcher) ignoredPath(abs string) bool {
	rel, err := filepath.Rel(fw.workspace, abs)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return false
	}
	parts := strings.Split(rel, "/")
	dir := fw.workspace
	for i, part := range parts {
		child := filepath.Join(dir, part)
		isDir := i < len(parts)-1
		if !isDir {
			if fi, statErr := os.Lstat(child); statErr == nil {
				isDir = fi.IsDir()
			}
		}
		if fw.stackFor(dir).IsIgnored(child, isDir) {
			return true
		}
		dir = child
	}
	return false
}

// stackFor returns the ignore.Stack in force inside dir, an absolute directory
// at or below the workspace root, memoising every level it builds. A path
// outside the workspace gets the empty stack, which ignores nothing.
func (fw *fsWatcher) stackFor(dir string) ignore.Stack {
	fw.ignoreMu.Lock()
	defer fw.ignoreMu.Unlock()
	if st, ok := fw.ignoreCache[dir]; ok {
		return st
	}
	rel, err := filepath.Rel(fw.workspace, dir)
	if err != nil {
		return nil
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return nil
	}
	st, ok := fw.ignoreCache[fw.workspace]
	if !ok {
		st = st.Load(fw.workspace)
		fw.storeStackLocked(fw.workspace, st)
	}
	if rel == "." {
		return st
	}
	cur := fw.workspace
	for _, part := range strings.Split(rel, "/") {
		cur = filepath.Join(cur, part)
		if cached, hit := fw.ignoreCache[cur]; hit {
			st = cached
			continue
		}
		st = st.Load(cur)
		fw.storeStackLocked(cur, st)
	}
	return st
}

// storeStackLocked memoises one directory's stack. Caller holds ignoreMu.
func (fw *fsWatcher) storeStackLocked(dir string, st ignore.Stack) {
	if fw.ignoreCache == nil {
		fw.ignoreCache = make(map[string]ignore.Stack)
	}
	if len(fw.ignoreCache) >= watcherIgnoreCacheMax {
		clear(fw.ignoreCache)
	}
	fw.ignoreCache[dir] = st
}

// dropIgnoreCache forgets every memoised stack. Called when an ignore file
// changes, which can alter the verdict for a whole subtree.
func (fw *fsWatcher) dropIgnoreCache() {
	fw.ignoreMu.Lock()
	fw.ignoreCache = nil
	fw.ignoreMu.Unlock()
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
