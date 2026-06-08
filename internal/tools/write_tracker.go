package tools

import (
	"os"
	"sync"
)

// WriteTracker records the set of file paths plumb has written during a single
// MCP session, so the dirty-guard never blocks a re-edit of a file plumb itself
// produced this session — while a file left dirty before plumb touched it
// (uncommitted work plumb did not create) still requires dirty_ok. The daemon
// creates one tracker per MCP connection so session A's writes don't leak into
// session B's guard; it is never process-global.
//
// Paths are canonicalised through lockPathKey (file:// strip, abs, symlink
// resolution, Clean) so a path and its symlink/relative spellings collapse to
// the same key — matching the per-path write lock.
//
// Concurrency: all methods are safe for concurrent use.
type WriteTracker struct {
	mu sync.Mutex
	// written maps a canonical path to the file's mtime (UnixNano) at the moment
	// plumb last wrote it this session. The mtime lets a later read detect a
	// concurrent external edit (a peer wrote the file after us); presence in the
	// map is what the dirty-guard consults.
	written map[string]int64
}

// NewWriteTracker returns an empty tracker. Pass nil into write-tool
// constructors when session-aware dirty tracking is not required (tests, dev):
// every method is nil-safe, and the guard then behaves as if plumb has written
// nothing this session (any dirty file blocks unless dirty_ok is set).
func NewWriteTracker() *WriteTracker {
	return &WriteTracker{written: make(map[string]int64)}
}

// Record marks path as written by plumb this session, capturing the file's
// current mtime so a later read can spot a concurrent external edit. Called
// after every successful write. nil-safe.
func (w *WriteTracker) Record(path string) {
	if w == nil {
		return
	}
	key := lockPathKey(path)
	var mtime int64
	if info, err := os.Stat(path); err == nil {
		mtime = info.ModTime().UnixNano()
	}
	w.mu.Lock()
	w.written[key] = mtime
	w.mu.Unlock()
}

// Reset forgets every recorded path. Called on a deliberate workspace re-pin so
// the dirty-guard starts clean for the new project — plumb has written nothing
// there yet, so a file already dirty in the new workspace must still require
// dirty_ok. nil-safe.
func (w *WriteTracker) Reset() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.written = make(map[string]int64)
	w.mu.Unlock()
}

// Wrote reports whether plumb has written path this session. nil-safe (false).
func (w *WriteTracker) Wrote(path string) bool {
	if w == nil {
		return false
	}
	key := lockPathKey(path)
	w.mu.Lock()
	_, ok := w.written[key]
	w.mu.Unlock()
	return ok
}

// WroteMtime returns the mtime (UnixNano) plumb recorded when it last wrote
// path this session, and whether plumb wrote it at all. A caller compares this
// against the file's current mtime to detect a concurrent external edit since
// the session's last write. nil-safe (0, false).
func (w *WriteTracker) WroteMtime(path string) (int64, bool) {
	if w == nil {
		return 0, false
	}
	key := lockPathKey(path)
	w.mu.Lock()
	mtime, ok := w.written[key]
	w.mu.Unlock()
	return mtime, ok
}
