package cli

// config_file_watch.go — the single-file watch both config watchers use
// (PLAN-485).
//
// The config watchers used to watch the DIRECTORY holding config.toml, to see
// an atomic save's inode swap. On kqueue (macOS, the BSDs) that is unsafe in a
// daemon: fsnotify mimics inotify by opening every file in a watched
// directory (O_EVTONLY), and closing any descriptor to a file drops every
// fcntl lock this process holds on it — the locks SQLite's WAL mode lives
// on. <workspace>/.plumb holds collab.db, topology.db and memory.db, and on a
// fresh macOS install the global config directory is the data directory that
// holds stats.db, session_state.db, history.db and collab-xproject.db. Each
// time a watcher dropped a per-file watch (a sidecar deleted, a watcher closed
// or recreated) the daemon silently lost its SQLite locks; the next process to
// open the database believed itself alone, checkpointed, and unlinked the WAL
// the daemon was still appending to. Everything written after that lived only
// in an unlinked file.
//
// So a config watcher now watches config.toml itself, which opens that one
// file and nothing else. What the directory watch used to see comes from two
// other places: a Remove or Rename on the watched file (an atomic save, a
// delete) detaches the watch, and the owner's next debounce fire re-attaches
// it to whatever now has the name; and a short stat tick attaches the file
// when it appears where there was none, and notices an inode swap that raised
// no event on the watched inode (a re-pointed symlink, a rename over a file
// that still has another link).

import (
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// configFileTick is how often a config watcher stats its file. It bounds how
// long a config.toml created where there was none (or swapped without an
// event) goes unnoticed; one stat a second per watched file costs nothing.
const configFileTick = time.Second

// configFileWatch keeps an fsnotify watch on exactly one file path. It is
// owned by one goroutine and needs no locking.
//
// It never calls the watcher's Remove. On kqueue, Remove of a file whose name
// has just moved to another inode fails half-way (ENOENT) and leaves fsnotify
// still listing the path against a closed descriptor, so the next Add of that
// path fails with EBADF and the watch is gone for good. Detaching therefore
// replaces the whole OS watcher instead, which leaves no state behind; it only
// happens when the file is swapped or deleted, so the cost is one kqueue per
// save, not per event.
type configFileWatch struct {
	path string
	// newWatcher builds each OS watcher: fsnotify.NewWatcher, or a test's
	// wrapper that captures each watcher it builds.
	newWatcher func() (*fsnotify.Watcher, error)
	// watcher is the current OS watcher; nil only when building a replacement
	// failed, until attach builds one. A nil watcher's channels never fire.
	watcher *fsnotify.Watcher
	// attached says watcher holds a watch on the inode that had path when
	// attach ran; id is that inode, for check to compare against.
	attached bool
	id       os.FileInfo
}

// attach watches path if it is not already watched and the file exists, and
// reports whether it attached just now. It stats BEFORE adding: a swap between
// the two then leaves the watch on the newer inode and id on the older, which
// check reads as a swap and answers with one extra reload. The other order
// would record the newer inode while watching the older, a mismatch nothing
// would ever notice.
func (c *configFileWatch) attach() bool {
	if c.attached {
		return false
	}
	fi, err := os.Stat(c.path)
	if err != nil {
		return false
	}
	if c.watcher == nil {
		if c.watcher, err = c.newWatcher(); err != nil {
			c.watcher = nil
			return false
		}
	}
	if err := c.watcher.Add(c.path); err != nil {
		// Whatever the Add left behind is not worth trusting; the next
		// attempt starts from a fresh watcher.
		c.replace()
		return false
	}
	c.attached, c.id = true, fi
	return true
}

// detach forgets the watch by replacing the OS watcher (see the type comment
// for why not Remove).
func (c *configFileWatch) detach() {
	if !c.attached {
		return
	}
	c.replace()
	c.attached, c.id = false, nil
}

// handleEvent and handleTick are onEvent and check for an owner's select
// loop: each re-arms the owner's debounce timer when the file changed.
func (c *configFileWatch) handleEvent(event fsnotify.Event, timer *time.Timer, debounce time.Duration) {
	if c.onEvent(event) {
		rearmProjectTimer(timer, debounce)
	}
}

func (c *configFileWatch) handleTick(timer *time.Timer, debounce time.Duration) {
	if c.check() {
		rearmProjectTimer(timer, debounce)
	}
}

// events and errs are the current watcher's channels for an owner's select; a
// nil channel (no watcher yet) never fires.
func (c *configFileWatch) events() <-chan fsnotify.Event {
	if c.watcher == nil {
		return nil
	}
	return c.watcher.Events
}

func (c *configFileWatch) errs() <-chan error {
	if c.watcher == nil {
		return nil
	}
	return c.watcher.Errors
}

// replace closes the current OS watcher and builds an empty one. If building
// fails, watcher stays nil and attach tries again.
func (c *configFileWatch) replace() {
	closeFSWatcher(c.watcher)
	w, err := c.newWatcher()
	if err != nil {
		w = nil
	}
	c.watcher = w
}

// onEvent reports whether event is a change to the watched file. A Remove or
// Rename means the name no longer leads to the watched inode, so the watch is
// detached for the owner's next attach to re-arm.
func (c *configFileWatch) onEvent(event fsnotify.Event) bool {
	if filepath.Clean(event.Name) != c.path {
		return false
	}
	if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		c.detach()
	}
	return event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) != 0
}

// check is the tick: it attaches a file that has appeared, and detaches a
// watch whose path now leads elsewhere or nowhere. It reports whether either
// happened, which the owner treats as a change.
func (c *configFileWatch) check() bool {
	if !c.attached {
		return c.attach()
	}
	fi, err := os.Stat(c.path)
	if err == nil && os.SameFile(fi, c.id) {
		return false
	}
	c.detach()
	return true
}
