package tools

import (
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
// Entries are keyed by lockPathKey, the key the write lock, the WriteTracker and
// the undo store agree on, so a file read through one spelling (a symlinked
// parent, macOS /tmp versus /private/tmp, a case variant on a volume that folds
// case) and written through another finds its read record. Keyed on the spelled
// path, the write guards saw "never read": the default staleness guard failed
// open over a peer's change and strict mode failed closed (issue #524).
//
// Concurrency: all methods are safe for concurrent use. The optional persist
// sink set by SetPersistSink is invoked outside the tracker lock, so it may do
// blocking I/O without stalling concurrent reads.
type ReadTracker struct {
	mu      sync.RWMutex
	entries map[string]readEntry // lockPathKey(path) → last-read state
	persist func(path string, mtime time.Time, sha string)
}

// ReadRecord is one path's recorded read state, used to rehydrate a tracker
// from a persisted store (e.g. after a daemon restart). Path is the identity key
// the tracker persists and Records returns, not necessarily a spelling to open:
// on a volume that folds case it is lowercased (see paths.CanonicalKey).
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
	// Resolve outside the lock: lockPathKey touches the filesystem.
	key := lockPathKey(path)
	r.mu.Lock()
	r.entries[key] = readEntry{mtime: mtime, sha: sha}
	persist := r.persist
	r.mu.Unlock()
	// Persist outside the lock; last-writer-wins races on the store are benign
	// and converge with the in-memory map.
	if persist != nil {
		persist(key, mtime, sha)
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
//
// Every record is re-keyed through lockPathKey, because a daemon that predates
// issue #524 persisted the path as the agent spelled it. Two spellings of one
// file then collapse onto one key, and one must win whatever order the store
// returned them in (see hydrateOutranks).
func (r *ReadTracker) Hydrate(records []ReadRecord) {
	if r == nil || len(records) == 0 {
		return
	}
	// Resolve outside the lock: lockPathKey touches the filesystem.
	loaded := make(map[string]hydratedRead, len(records))
	for _, rec := range records {
		key := lockPathKey(rec.Path)
		c := hydratedRead{readEntry: readEntry{mtime: rec.Mtime, sha: rec.SHA}, canonical: rec.Path == key}
		if prev, dup := loaded[key]; dup && !hydrateOutranks(c, prev) {
			continue
		}
		loaded[key] = c
	}
	r.mu.Lock()
	for key, e := range loaded {
		r.entries[key] = e.readEntry
	}
	r.mu.Unlock()
}

// hydratedRead is one row on its way into Hydrate, remembering whether it was
// already stored under its key.
type hydratedRead struct {
	readEntry
	canonical bool
}

// hydrateOutranks reports whether c should replace prev when both name one file.
// A row already stored under its canonical key wins: a daemon with this keying
// writes nothing else, so a row spelled any other way was left by an older
// daemon and predates it. File mtime cannot decide that pair, because
// mtime-preserving tools (cp -p, rsync -t) move it backwards, and an older read
// winning would make the guards refuse a file the session did read, on every
// restart. Between two rows of the same kind, the later file mtime wins: the
// newest version seen.
func hydrateOutranks(c, prev hydratedRead) bool {
	if c.canonical != prev.canonical {
		return c.canonical
	}
	return c.mtime.After(prev.mtime)
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
	key := lockPathKey(path)
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.entries[key].mtime
}

// recorded returns the full last-read state for path and whether the session
// has read it on this tracker. nil-safe (returns ok=false).
func (r *ReadTracker) recorded(path string) (readEntry, bool) {
	if r == nil {
		return readEntry{}, false
	}
	key := lockPathKey(path)
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[key]
	return e, ok
}

// Records snapshots every recorded read. It exists so a connection's reads can
// seed the shard of the agent that made them when the connection turns shared:
// the connection-level tracker persists under the empty agent id, so a fresh
// shard rehydrating only its own rows would start empty and every strict-mode
// edit the agent had in flight would fail "has not been read". nil-safe.
func (r *ReadTracker) Records() []ReadRecord {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ReadRecord, 0, len(r.entries))
	for path, e := range r.entries {
		out = append(out, ReadRecord{Path: path, Mtime: e.mtime, SHA: e.sha})
	}
	return out
}
