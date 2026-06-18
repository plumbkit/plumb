package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

// maxUndoSnapshotBytes caps the size of a single pre-write snapshot kept for
// undo_edit. A file whose pre-write content exceeds this is not snapshotted
// (undo is then unavailable for it), bounding per-entry memory.
const maxUndoSnapshotBytes = 1 << 20 // 1 MiB

// maxUndoEntries caps how many distinct paths the store retains; the oldest is
// evicted when the cap is reached, bounding total memory across a long session.
const maxUndoEntries = 64

// undoSnapshot records the state needed to revert plumb's most recent write to
// one path: the content before the write (to restore) and a hash of what plumb
// wrote (to detect an external change since, so an undo never silently clobbers
// a peer's edit).
type undoSnapshot struct {
	before        string // content before the write; "" when the write created the file
	existedBefore bool   // did the file exist before the write?
	afterSHA      string // sha256 hex of the content plumb wrote
	tool          string // the tool that made the write (for the response)
	seq           uint64 // insertion order, for oldest-first eviction
}

// UndoStore holds the single most recent revertible write per path for one MCP
// session, powering undo_edit. The daemon creates one per connection so one
// session's history never leaks into another's; it is never process-global.
//
// Concurrency: all methods are safe for concurrent use.
type UndoStore struct {
	mu    sync.Mutex
	snaps map[string]undoSnapshot
	seq   uint64
}

// NewUndoStore returns an empty store. Pass nil into WriteDeps when undo capture
// is not required (tests, dev); every method is nil-safe.
func NewUndoStore() *UndoStore {
	return &UndoStore{snaps: make(map[string]undoSnapshot)}
}

// Record stores the snapshot for path, replacing any previous one (single-level
// undo per path) and evicting the oldest entry when the cap is reached. Paths
// are canonicalised through lockPathKey, matching the per-path write lock and
// the write/read trackers. nil-safe.
func (u *UndoStore) Record(path string, snap undoSnapshot) {
	if u == nil {
		return
	}
	key := lockPathKey(path)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.seq++
	snap.seq = u.seq
	if _, exists := u.snaps[key]; !exists && len(u.snaps) >= maxUndoEntries {
		u.evictOldestLocked()
	}
	u.snaps[key] = snap
}

// evictOldestLocked removes the entry with the smallest seq. Caller holds mu.
func (u *UndoStore) evictOldestLocked() {
	var oldestKey string
	var oldestSeq uint64
	first := true
	for k, s := range u.snaps {
		if first || s.seq < oldestSeq {
			oldestKey, oldestSeq, first = k, s.seq, false
		}
	}
	if !first {
		delete(u.snaps, oldestKey)
	}
}

// Peek returns the snapshot for path without removing it. nil-safe (zero, false).
func (u *UndoStore) Peek(path string) (undoSnapshot, bool) {
	if u == nil {
		return undoSnapshot{}, false
	}
	key := lockPathKey(path)
	u.mu.Lock()
	defer u.mu.Unlock()
	s, ok := u.snaps[key]
	return s, ok
}

// Take returns the snapshot for path and removes it, so a single write is
// undoable only once (a fresh write re-arms it). nil-safe (zero, false).
func (u *UndoStore) Take(path string) (undoSnapshot, bool) {
	if u == nil {
		return undoSnapshot{}, false
	}
	key := lockPathKey(path)
	u.mu.Lock()
	defer u.mu.Unlock()
	s, ok := u.snaps[key]
	if ok {
		delete(u.snaps, key)
	}
	return s, ok
}

// Reset forgets every recorded snapshot — called on a deliberate workspace
// re-pin so undo history never crosses projects. nil-safe.
func (u *UndoStore) Reset() {
	if u == nil {
		return
	}
	u.mu.Lock()
	u.snaps = make(map[string]undoSnapshot)
	u.mu.Unlock()
}

// sha256OfString returns the hex-encoded SHA-256 of s.
func sha256OfString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
