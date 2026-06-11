package tools

import (
	"path/filepath"
	"sync"
	"time"
)

// ReadTracker records what read_file observed for each path the session has
// read — the mtime and the content SHA-256. The daemon creates one tracker per
// MCP connection so session A's reads don't leak into session B's strict-mode
// check (which was a known limitation of the process-global map used in
// 0.5.1/0.5.2).
//
// The recorded SHA lets the staleness guard catch a peer write that left the
// mtime unchanged (a same-tick write, or a tool that preserves mtime such as
// `cp -p` or some formatters) — a change an mtime-only comparison misses.
//
// Concurrency: all methods are safe for concurrent use.
type ReadTracker struct {
	mu      sync.RWMutex
	entries map[string]readEntry // filepath.Clean(path) → last-read state
}

// readEntry is the state read_file observed for a path: its mtime and the
// hex-encoded SHA-256 of its content (empty when hashing failed at read time).
type readEntry struct {
	mtime time.Time
	sha   string
}

// NewReadTracker returns an empty tracker. Pass nil into write/edit-tool
// constructors when strict-mode tracking is not required (tests, dev).
func NewReadTracker() *ReadTracker {
	return &ReadTracker{entries: make(map[string]readEntry)}
}

// Record stores the mtime and content SHA read_file observed for path. Called
// after every successful read; sha may be empty when hashing failed. nil-safe.
func (r *ReadTracker) Record(path string, mtime time.Time, sha string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.entries[filepath.Clean(path)] = readEntry{mtime: mtime, sha: sha}
	r.mu.Unlock()
}

// Reset forgets every recorded read. Called on a deliberate workspace re-pin so
// strict-mode read tracking starts clean for the new project: a read of a file
// in the old workspace must not satisfy the read-before-edit check for a
// different project. nil-safe.
func (r *ReadTracker) Reset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.entries = make(map[string]readEntry)
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
	return r.entries[filepath.Clean(path)].mtime
}

// recorded returns the full last-read state for path and whether the session
// has read it on this tracker. nil-safe (returns ok=false).
func (r *ReadTracker) recorded(path string) (readEntry, bool) {
	if r == nil {
		return readEntry{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[filepath.Clean(path)]
	return e, ok
}
