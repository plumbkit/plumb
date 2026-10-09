// Package fswatch watches a workspace tree recursively and delivers per-path
// change events. It is the one place plumb chooses an OS file-watching backend,
// and the choice is constrained by a property no event test can see: the
// backend must never open the files it watches.
//
// On macOS, closing ANY descriptor to a file releases every fcntl lock the
// process holds on it. The daemon keeps SQLite databases open under each
// workspace's .plumb directory, so a watcher that opens a descriptor per file
// (kqueue, as sgtdi/fswatcher and fsnotify both use on darwin without cgo)
// strips SQLite's locks the moment it closes them. The next process to open the
// database then believes it is alone, checkpoints, and unlinks the WAL the
// daemon is still writing (PLAN-488). Filtering events cannot help: the
// descriptor exists whether or not its events are delivered.
//
// The backends are therefore:
//
//   - darwin: FSEvents through plumb's own binding (fsevents_darwin.go), which
//     calls CoreServices via purego and so needs no cgo. One recursive stream
//     per root; it opens nothing in the tree. Release builds (CGO_ENABLED=0)
//     and local cgo builds run the same code.
//   - linux, windows: github.com/sgtdi/fswatcher (inotify,
//     ReadDirectoryChangesW). Neither opens the files it watches.
//   - everything else: no watcher. New returns an error that names the
//     hazard, and callers fall back to periodic resync. The BSDs only offer
//     kqueue, which would reintroduce the lock loss.
//
// Every backend keeps the same contract, and the tests hold each to it:
//
//   - It never opens a file under the root.
//   - Event paths are absolute, under the root as the caller spelled it, and
//     name the entry that changed: a symlink is reported under its own name,
//     not its target's.
//   - Whenever it cannot report changes one by one (a queue overflow, a
//     rescan request, a directory too large to expand, the root itself being
//     removed or replaced, a full buffer), it says so on Lost instead of
//     staying silent.
//   - It keeps watching the root PATH: a root directory renamed away and
//     recreated, or replaced by another, is watched again.
//   - New returns only once the root is watched, so a consumer's first full
//     scan made after it cannot miss a change. A Lost that follows a root's
//     return is likewise signalled only once the new watch is in place.
//   - A directory that arrives whole (created, or moved in) is reported with
//     its contents. A directory moved away is reported under its own name
//     only: no OS reports the entries it took with it, so a consumer drops
//     what it knew below that name itself.
package fswatch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Op is a set of change kinds, as a bitmask. A single event may carry several
// bits when the OS coalesces changes to one path.
type Op uint8

const (
	// Create reports that a file or directory appeared.
	Create Op = 1 << iota
	// Write reports that a file's contents changed.
	Write
	// Remove reports that a file or directory was removed.
	Remove
	// Rename reports that a file or directory was renamed or moved, under
	// either its old or its new name.
	Rename
	// Chmod reports a permission, ownership or extended-attribute change.
	Chmod
)

// Has reports whether op contains any bit of target.
func (op Op) Has(target Op) bool { return op&target != 0 }

// Event is one change notification for one path.
type Event struct {
	// Path is absolute and lies under the root passed to New, spelled with
	// that root's prefix even when the OS reports the resolved form.
	Path string
	// Op is the union of every change seen for Path since its last delivery.
	Op Op
}

// Options configures a Watcher.
type Options struct {
	// Cooldown collapses a burst of changes to one path into one event,
	// delivered once the path has been quiet for this long. Zero delivers each
	// change as it arrives.
	Cooldown time.Duration
	// ExcludeRegex, when non-empty, drops every event whose absolute path or
	// base name it matches, before debouncing. It filters events only: it never
	// narrows what the OS watches. Build one with ExcludeDirsRegex.
	ExcludeRegex string
}

// ErrUnsupported is returned by New on a platform with no backend that is safe
// to run next to the daemon's SQLite databases.
var ErrUnsupported = errors.New("fswatch: no safe recursive watcher on this platform")

// eventBuffer is how many undelivered events a Watcher holds for a slow
// consumer before it reports loss instead of blocking the OS stream.
const eventBuffer = 4096

// Watcher watches one root recursively.
//
// Concurrency: New starts the backend's goroutines; Events, Lost and Failed may
// be read from any goroutine. The backend is the only sender on Events and Lost,
// and it never blocks on a consumer: a full Events buffer is reported on Lost
// instead. Close stops the backend, waits for its goroutines, then closes Events
// and Lost. Close is safe to call more than once and from any goroutine.
type Watcher struct {
	root   string
	events chan Event
	lost   chan struct{}

	// failed is closed, once, by a backend that has stopped for good. Only the
	// sgtdi supervisor can (backend_sgtdi.go); FSEvents streams do not die.
	failed chan struct{}

	exclude     *regexp.Regexp // Options.ExcludeRegex, nil when empty
	expandLimit int            // maxExpand, smaller in tests

	done      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
	stop      func() // set by the backend; stops the OS watcher
}

// New starts watching root and every directory beneath it. An error means no
// watcher is running; the caller is expected to fall back to periodic
// reconciliation.
func New(root string, opts Options) (*Watcher, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("fswatch: %s: %w", root, err)
	}
	// Checked here rather than left to the backend: FSEvents happily watches a
	// path that does not exist, while inotify refuses, and a caller should get
	// the same answer on every platform.
	if fi, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("fswatch: %w", err)
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("fswatch: %s is not a directory", abs)
	}
	if opts.Cooldown < 0 {
		return nil, fmt.Errorf("fswatch: negative cooldown %v", opts.Cooldown)
	}
	var exclude *regexp.Regexp
	if opts.ExcludeRegex != "" {
		if exclude, err = regexp.Compile(opts.ExcludeRegex); err != nil {
			return nil, fmt.Errorf("fswatch: exclude regex: %w", err)
		}
	}
	w := &Watcher{
		root:        filepath.Clean(abs),
		events:      make(chan Event, eventBuffer),
		lost:        make(chan struct{}, 1),
		failed:      make(chan struct{}),
		exclude:     exclude,
		expandLimit: maxExpand,
		done:        make(chan struct{}),
	}
	if err := w.startBackend(opts); err != nil {
		return nil, err
	}
	return w, nil
}

// Events delivers change notifications. It is closed by Close.
func (w *Watcher) Events() <-chan Event { return w.events }

// Lost receives a value whenever events may have been lost: the OS dropped or
// coalesced beyond recovery (an inotify queue overflow, FSEvents asking for a
// subtree rescan, any error the backend reports), or a consumer fell so far
// behind that Events filled. Signals coalesce, so one value can stand for many
// losses. The only sound response is to reconcile the whole tree. It is closed
// by Close.
func (w *Watcher) Lost() <-chan struct{} { return w.lost }

// Failed is closed if the backend stops for good on its own: no further events
// will arrive from this Watcher, ever. It is a degraded state, not a loss to
// reconcile once. Lost is signalled too, but a single reconcile restores
// nothing after it, so a consumer relying on events for freshness must fall back
// to something periodic. A lost root is NOT a failure: the Watcher waits for the
// root to return. Close does not close Failed.
func (w *Watcher) Failed() <-chan struct{} { return w.failed }

// Close stops watching and waits for the backend to finish.
func (w *Watcher) Close() {
	w.closeOnce.Do(func() {
		close(w.done)
		if w.stop != nil {
			w.stop()
		}
		w.wg.Wait()
		close(w.events)
		close(w.lost)
	})
}

// deliver hands ev to the consumer without ever blocking the backend. A full
// buffer means the consumer must reconcile, which is what Lost asks for.
func (w *Watcher) deliver(ev Event) {
	select {
	case w.events <- ev:
	default:
		w.signalLost()
	}
}

// signalLost records that events may have been lost. Coalescing: if a signal
// is already pending, this one is folded into it.
func (w *Watcher) signalLost() {
	select {
	case w.lost <- struct{}{}:
	default:
	}
}

// ExcludeDirsRegex builds an exclusion for Options.ExcludeRegex that drops
// events under any directory named in dirs, and under any dot-prefixed
// directory, at any depth inside root.
//
// It is ANCHORED AT root. The exclusion is tested against absolute event paths,
// so an unanchored dot-directory branch also matches the directories a
// workspace merely LIVES under: a checkout at ~/.config/app, or a test's
// t.TempDir() under a dot-prefixed cache, had every event excluded and the
// watcher delivered nothing at all.
//
// The dot branch requires a TRAILING SEPARATOR, so it excludes dot
// DIRECTORIES and not a dot FILE at the end of a path. The exclusion is also
// tested against the bare base name, and a `(/|$)` ending matched the base name
// ".gitignore", so no ignore-file event was ever delivered. The anchored root
// prefix means no bare base name can match this pattern at all.
//
// Both separators are accepted because event paths use the platform's and the
// quoted root carries whichever filepath.Clean produced.
func ExcludeDirsRegex(root string, dirs ...string) string {
	const sep = `[\\/]`
	quoted := make([]string, len(dirs))
	for i, d := range dirs {
		quoted[i] = regexp.QuoteMeta(d)
	}
	prefix := `^` + regexp.QuoteMeta(filepath.Clean(root)) + sep + `(?:.*` + sep + `)?`
	if len(quoted) == 0 {
		return prefix + `\.[^\\/]+` + sep
	}
	return prefix + `(?:(?:` + strings.Join(quoted, "|") + `)(?:` + sep + `|$)|\.[^\\/]+` + sep + `)`
}
