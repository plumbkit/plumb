package tools

import (
	"path/filepath"
	"sync"
	"time"
)

// ReadTracker records the mtime that read_file observed for each path the
// session has read. The daemon creates one tracker per MCP connection so
// session A's reads don't leak into session B's strict-mode check (which
// was a known limitation of the process-global map used in 0.5.1/0.5.2).
//
// Concurrency: all methods are safe for concurrent use.
type ReadTracker struct {
	mu     sync.RWMutex
	mtimes map[string]time.Time // filepath.Clean(path) → mtime
}

// NewReadTracker returns an empty tracker. Pass nil into write/edit-tool
// constructors when strict-mode tracking is not required (tests, dev).
func NewReadTracker() *ReadTracker {
	return &ReadTracker{mtimes: make(map[string]time.Time)}
}

// Record stores the mtime read_file observed for path. Called after every
// successful read. nil-safe.
func (r *ReadTracker) Record(path string, mtime time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.mtimes[filepath.Clean(path)] = mtime
	r.mu.Unlock()
}

// Mtime returns the mtime that was last recorded for path, or the zero
// time.Time if read_file has never been called for it on this tracker.
// nil-safe (returns zero).
func (r *ReadTracker) Mtime(path string) time.Time {
	if r == nil {
		return time.Time{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.mtimes[filepath.Clean(path)]
}
