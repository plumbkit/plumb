package fswatch

import (
	"os"
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
// .git) but never downgrades a "worktree" one mid-removal, and neither does the
// directory's own Remove. An event that creates or renames into a recorded
// worktree's path re-decides it and supersedes the record, which can be stale.
// Lost events re-decide every verdict against fresh bookkeeping, since what was
// lost may have replaced a directory.

// worktreeCacheMax bounds each verdict set. Past it the "not a worktree" set is
// dropped wholesale and rebuilt lazily; the "worktree" set — few, and losing
// one mid-removal is the leak this file prevents — drops only the directories
// that no longer exist.
const worktreeCacheMax = 4096

// worktreeSkip answers "is this path inside a linked worktree of root".
//
// Concurrency: safe for concurrent use; mu guards every field below it.
type worktreeSkip struct {
	root string

	mu sync.Mutex
	// worktree and ordinary are the cached verdicts for directories below root,
	// kept apart so an arriving directory can drop the worktree verdicts beneath
	// it without scanning every ordinary one.
	worktree map[string]struct{}
	ordinary map[string]struct{}
	recorded map[string]struct{} // worktrees git's bookkeeping records, in root's spelling
	// superseded are recorded worktrees whose path an event has since created or
	// renamed into: the record may be stale (an admin directory outlives an
	// `rm -rf` until `git worktree prune`), so only a live verification counts
	// there, until a .git that verifies appears.
	superseded map[string]struct{}
	// ident is the identity of each directory a worktree verdict or record was
	// formed for, taken when it was formed: an arrival replaces a belief only
	// where a DIFFERENT directory now stands (arrivalLocked).
	ident map[string]os.FileInfo

	// dirty is set by lost and consumed by the next lookup, which re-reads the
	// bookkeeping and re-decides every verdict once: a burst of loss signals
	// costs one refresh, not one per signal.
	dirty atomic.Bool

	// isWorktree, listWorktrees, stat and sameFile are paths.LinkedWorktreeOf,
	// paths.LinkedWorktreesOf, os.Lstat and os.SameFile: seams for tests that
	// count or fake lookups and directories.
	isWorktree    func(root, dir string) bool
	listWorktrees func(root string) []string
	stat          func(string) (os.FileInfo, error)
	sameFile      func(a, b os.FileInfo) bool
}

func newWorktreeSkip(root string) *worktreeSkip {
	s := &worktreeSkip{root: root, isWorktree: paths.LinkedWorktreeOf, listWorktrees: paths.LinkedWorktreesOf, stat: os.Lstat, sameFile: os.SameFile}
	s.resetLocked()
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
	// Remembered: a worktree that arrives is not expanded, so nothing inside it
	// may ever be looked up before it is removed — by when its .git and git's
	// record of it can both be gone.
	v := s.decideLocked(dir)
	if v {
		s.rememberLocked(dir, true)
	}
	return v
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
		clear(s.worktree)
		clear(s.ordinary)
		s.refreshLocked()
	}
}

// resetLocked empties every verdict. s.mu must be held (or s unshared).
func (s *worktreeSkip) resetLocked() {
	s.worktree, s.ordinary, s.superseded = map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}
	s.ident = map[string]os.FileInfo{}
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
// that creates or renames into path re-decides path itself. A .git event
// re-reads the bookkeeping and re-decides the directory holding it, unless that
// directory is already a worktree: its .git going away is how a removal starts.
// s.mu must be held.
func (s *worktreeSkip) forgetLocked(path string, op Op) {
	// Only a directory arriving at the path re-decides it and supersedes its
	// record. Its own Remove does neither: sgtdi delivers a burst's paths in no
	// set order, at least its cooldown after the fact (by when git has deleted
	// the admin directory too), so the directory's Remove can come before its
	// children's, and the rest of the removal must stay skipped.
	if op.Has(Create | Rename) {
		s.arrivalLocked(path)
	}
	if filepath.Base(path) == ".git" {
		dir := filepath.Dir(path)
		_, isWorktree := s.worktree[dir]
		if dir != s.root && !isWorktree { // the root's own .git changes with every git command
			delete(s.ordinary, dir)
			s.refreshLocked()
		}
	}
}

// arrivalLocked re-decides what a directory arriving at path replaces: the
// worktree verdicts and records at and beneath path whose directory is now a
// DIFFERENT one (PLAN-558). An ordinary tree moved in where a worktree's parent
// was holds no worktree, but the old verdict for <path>/<wt> would otherwise
// silence it, and the arrival's expansion judges its subdirectories with
// arrived(), which a superseded record no longer answers.
//
// Identity, not the event, decides. inotify reports a directory moving AWAY as
// Remove|Rename, and one moved away and back is the same directory: dropping
// its beliefs then would leak the rest of a removal (#703's review measured
// thousands of events). So nothing happens unless a directory now stands at
// path, and a belief is dropped only where its directory still exists and is
// not the one it was formed for. A real worktree in the arriving tree verifies
// again. s.mu must be held.
func (s *worktreeSkip) arrivalLocked(path string) {
	fi, err := s.stat(path)
	if err != nil || !fi.IsDir() {
		return // nothing arrived: a move away, or a file
	}
	delete(s.ordinary, path)
	prefix := path + string(filepath.Separator)
	under := func(d string) bool { return d == path || strings.HasPrefix(d, prefix) }
	for d := range s.worktree {
		if under(d) && s.replacedLocked(d) {
			delete(s.worktree, d)
		}
	}
	for d := range s.recorded {
		if under(d) && s.replacedLocked(d) {
			s.superseded[d] = struct{}{}
		}
	}
}

// replacedLocked reports whether the directory at d is not the one a belief
// about d was formed for. A d that is gone is not replaced (it is being
// removed, and its belief must cover the rest of the removal). s.mu must be held.
func (s *worktreeSkip) replacedLocked(d string) bool {
	cur, err := s.stat(d)
	if err != nil {
		return false
	}
	old, ok := s.ident[d]
	return !ok || !s.sameFile(old, cur)
}

// identifyLocked records the identity of the directory a belief about d is
// formed for, when it exists. s.mu must be held.
func (s *worktreeSkip) identifyLocked(d string) {
	if fi, err := s.stat(d); err == nil {
		s.ident[d] = fi
	}
}

// verdictLocked is dir's cached verdict, decided on a miss. s.mu must be held.
func (s *worktreeSkip) verdictLocked(dir string) bool {
	if _, ok := s.worktree[dir]; ok {
		return true
	}
	if _, ok := s.ordinary[dir]; ok {
		return false
	}
	v := s.decideLocked(dir)
	s.rememberLocked(dir, v)
	return v
}

// rememberLocked caches dir's verdict, within worktreeCacheMax. s.mu must be held.
func (s *worktreeSkip) rememberLocked(dir string, isWorktree bool) {
	if !isWorktree {
		if len(s.ordinary) >= worktreeCacheMax {
			clear(s.ordinary)
		}
		s.ordinary[dir] = struct{}{}
		return
	}
	delete(s.ordinary, dir)
	if _, ok := s.worktree[dir]; ok {
		return
	}
	if len(s.worktree) >= worktreeCacheMax {
		for d := range s.worktree {
			if _, err := s.stat(d); err != nil {
				delete(s.worktree, d)
			}
		}
	}
	s.worktree[dir] = struct{}{}
	s.identifyLocked(dir)
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
	for d := range s.recorded {
		if _, ok := s.ident[d]; !ok {
			s.identifyLocked(d)
		}
	}
	for d := range s.ident {
		_, rec := s.recorded[d]
		_, wt := s.worktree[d]
		if !rec && !wt {
			delete(s.ident, d)
		}
	}
}
