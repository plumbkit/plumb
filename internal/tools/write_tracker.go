package tools

import "sync"

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
	mu      sync.Mutex
	written map[string]struct{}
}

// NewWriteTracker returns an empty tracker. Pass nil into write-tool
// constructors when session-aware dirty tracking is not required (tests, dev):
// every method is nil-safe, and the guard then behaves as if plumb has written
// nothing this session (any dirty file blocks unless dirty_ok is set).
func NewWriteTracker() *WriteTracker {
	return &WriteTracker{written: make(map[string]struct{})}
}

// Record marks path as written by plumb this session. Called after every
// successful write. nil-safe.
func (w *WriteTracker) Record(path string) {
	if w == nil {
		return
	}
	key := lockPathKey(path)
	w.mu.Lock()
	w.written[key] = struct{}{}
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
	w.written = make(map[string]struct{})
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
