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
// Concurrency: all methods are safe for concurrent use. The optional persist
// sink set by SetPersistSink is invoked outside the tracker lock, so it may do
// blocking I/O without stalling concurrent reads.
type ReadTracker struct {
	mu      sync.RWMutex
	entries map[string]readEntry // filepath.Clean(path) → last-read state
	persist func(path string, mtime time.Time, sha string)
}

// ReadRecord is one path's recorded read state, used to rehydrate a tracker
// from a persisted store (e.g. after a daemon restart).
type ReadRecord struct {
	Path  string
	Mtime time.Time
	SHA   string
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
	clean := filepath.Clean(path)
	r.mu.Lock()
	r.entries[clean] = readEntry{mtime: mtime, sha: sha}
	persist := r.persist
	r.mu.Unlock()
	// Persist outside the lock; last-writer-wins races on the store are benign
	// and converge with the in-memory map.
	if persist != nil {
		persist(clean, mtime, sha)
	}
}

// SetPersistSink installs a sink called after every Record with the recorded
// (path, mtime, sha), so reads can be mirrored to a durable store. The sink
// runs outside the tracker lock. Pass nil to disable. nil-safe.
func (r *ReadTracker) SetPersistSink(fn func(path string, mtime time.Time, sha string)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.persist = fn
	r.mu.Unlock()
}

// Hydrate loads previously-recorded reads into the tracker without firing the
// persist sink (the records came from the store; re-persisting them is wasted
// work). Existing entries for the same paths are overwritten. nil-safe.
func (r *ReadTracker) Hydrate(records []ReadRecord) {
	if r == nil || len(records) == 0 {
		return
	}
	r.mu.Lock()
	for _, rec := range records {
		r.entries[filepath.Clean(rec.Path)] = readEntry{mtime: rec.Mtime, sha: rec.SHA}
	}
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
