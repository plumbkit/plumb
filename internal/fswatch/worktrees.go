package fswatch

import (
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/plumbkit/plumb/internal/paths"
)

// worktrees.go — Options.SkipLinkedWorktrees: a watcher that drops every event
// inside a linked worktree of its root. Agents create and remove worktrees
// inside a workspace by the dozen, each a full copy of the tree. Without this,
// every checkout or removal was a burst of thousands of events (and, for a
// worktree moved in whole, an expansion of all of it) that filled the event
// buffer and reported loss: on plumb-ops, a shared watcher over the agents'
// worktrees logged 583k lost-event warnings in a day.
//
// A directory is a worktree when it verifies as one (paths.LinkedWorktreeOf) or
// when git's own bookkeeping records it (paths.LinkedWorktreesOf: the admin
// directories' back-links, read when the watcher starts and again whenever a
// .git entry changes or events are lost). The second half is what keeps a
// REMOVAL quiet: `git worktree remove` and `rm -rf` delete the checkout's .git
// along with everything else, often first, and the admin directory outlives
// the checkout.
//
// Verdicts are cached per directory, so a steady stream costs a map lookup per
// ancestor. A "worktree" verdict is dropped only by an event that creates,
// removes or renames the directory itself; a .git event re-decides a "not a
// worktree" verdict (`git worktree add` creates the directory before it writes
// .git) but never downgrades a "worktree" one mid-removal. An event that
// creates a recorded worktree's path supersedes the record, which can be stale.
// Lost events re-decide every verdict against fresh bookkeeping, since what was
// lost may have replaced a directory.

// worktreeCacheMax bounds the "not a worktree" verdicts; past it they are
// dropped wholesale and rebuilt lazily. "Worktree" verdicts are never evicted:
// there are few, and losing one mid-removal is the leak this file prevents.
const worktreeCacheMax = 4096

// worktreeSkip answers "is this path inside a linked worktree of root".
//
// Concurrency: safe for concurrent use; mu guards every field below it.
type worktreeSkip struct {
	root string

	mu       sync.Mutex
	known    map[string]bool     // directory below root -> it is a linked worktree of root
	recorded map[string]struct{} // worktrees git's bookkeeping records, in root's spelling
	// superseded are recorded worktrees whose path an event has since created or
	// renamed into: the record may be stale (an admin directory outlives an
	// `rm -rf` until `git worktree prune`), so only a live verification counts
	// there, until a .git that verifies appears.
	superseded map[string]struct{}

	// dirty is set by lost and consumed by the next lookup, which re-reads the
	// bookkeeping and re-decides every verdict once: a burst of loss signals
	// costs one refresh, not one per signal.
	dirty atomic.Bool

	// isWorktree and listWorktrees are paths.LinkedWorktreeOf and
	// paths.LinkedWorktreesOf, seams for tests that count or fake lookups.
	isWorktree    func(root, dir string) bool
	listWorktrees func(root string) []string
}

func newWorktreeSkip(root string) *worktreeSkip {
	s := &worktreeSkip{root: root, known: map[string]bool{}, superseded: map[string]struct{}{}, isWorktree: paths.LinkedWorktreeOf, listWorktrees: paths.LinkedWorktreesOf}
	s.refreshLocked()
	return s
}

// inside reports whether path, the subject of an event with op, lies inside a
// linked worktree of root: whether a directory strictly between root and path
// is one. The root itself is never judged (a workspace that IS a linked
// worktree is watched), and nor is path itself, which may be a file; arrived
// reports on a directory that just arrived.
func (s *worktreeSkip) inside(path string, op Op) bool {
	if s == nil {
		return false
	}
	rel, ok := s.below(path)
	if !ok {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settleLocked()
	s.forgetLocked(path, op)
	dir := s.root
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, part)
		if s.verdictLocked(dir) {
			return true
		}
	}
	return false
}

// arrived reports whether dir, a directory that just arrived below root, is
// itself a linked worktree — which is then not expanded.
func (s *worktreeSkip) arrived(dir string) bool {
	if s == nil {
		return false
	}
	if _, ok := s.below(dir); !ok {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settleLocked()
	return s.decideLocked(dir)
}

// lost marks every verdict for re-deciding: the events that would have told
// this watcher a worktree was removed and replaced (or a directory became one)
// may be among those lost. It is O(1); the next lookup does the work.
func (s *worktreeSkip) lost() {
	if s != nil {
		s.dirty.Store(true)
	}
}

// settleLocked applies a pending lost: re-read the bookkeeping and drop every
// verdict. s.mu must be held.
func (s *worktreeSkip) settleLocked() {
	if s.dirty.Swap(false) {
		s.refreshLocked()
		clear(s.known)
	}
}

// below is path relative to root, when it lies strictly below it.
func (s *worktreeSkip) below(path string) (string, bool) {
	rel, err := filepath.Rel(s.root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// forgetLocked drops what an event at path with op may have changed. An event
// that creates, removes or renames path re-decides path itself. A .git event
// re-reads the bookkeeping and re-decides the directory holding it, unless that
// directory is already a worktree: its .git going away is how a removal starts.
// s.mu must be held.
func (s *worktreeSkip) forgetLocked(path string, op Op) {
	if op.Has(Create | Remove | Rename) {
		delete(s.known, path)
		if _, ok := s.recorded[path]; ok {
			s.superseded[path] = struct{}{}
		}
	}
	if filepath.Base(path) == ".git" {
		dir := filepath.Dir(path)
		if dir != s.root && !s.known[dir] { // the root's own .git changes with every git command
			delete(s.known, dir)
			s.refreshLocked()
		}
	}
}

// verdictLocked is dir's cached verdict, decided on a miss. s.mu must be held.
func (s *worktreeSkip) verdictLocked(dir string) bool {
	if v, ok := s.known[dir]; ok {
		return v
	}
	if len(s.known) >= worktreeCacheMax {
		for d, v := range s.known {
			if !v {
				delete(s.known, d)
			}
		}
	}
	v := s.decideLocked(dir)
	s.known[dir] = v
	return v
}

// decideLocked decides dir from the disk: a verified worktree, or one git's
// bookkeeping records. s.mu must be held.
func (s *worktreeSkip) decideLocked(dir string) bool {
	if s.isWorktree(s.root, dir) {
		delete(s.superseded, dir) // a worktree again: its record can be trusted
		return true
	}
	_, recorded := s.recorded[dir]
	_, superseded := s.superseded[dir]
	return recorded && !superseded
}

// refreshLocked re-reads git's bookkeeping, keyed in the root's own spelling
// (event paths use it; the bookkeeping is canonical). s.mu must be held.
func (s *worktreeSkip) refreshLocked() {
	canonRoot := paths.Canonical(s.root)
	s.recorded = map[string]struct{}{}
	for _, wt := range s.listWorktrees(s.root) {
		if rel, err := filepath.Rel(canonRoot, wt); err == nil {
			s.recorded[filepath.Join(s.root, rel)] = struct{}{}
		}
	}
	for d := range s.superseded {
		if _, ok := s.recorded[d]; !ok {
			delete(s.superseded, d) // no longer recorded: nothing to supersede
		}
	}
}
