package fswatch

import (
	"path/filepath"
	"strings"
	"sync"

	"github.com/plumbkit/plumb/internal/paths"
)

// worktrees.go — Options.SkipLinkedWorktrees: a watcher that drops every event
// inside a linked worktree of its root (paths.LinkedWorktreeOf). Agents create
// and remove worktrees inside a workspace by the dozen, each a full copy of the
// tree. Without this, every checkout or removal was a burst of thousands of
// events (and, for a worktree moved in whole, an expansion of all of it) that
// filled the event buffer and reported loss: on plumb-ops, a shared watcher over
// the agents' worktrees logged 583k lost-event warnings in a day.
//
// The verdict is per directory and cached, so a steady stream costs a map lookup
// per ancestor. It is re-decided whenever an event touches the directory itself
// or its .git entry: `git worktree add` creates the directory before it writes
// .git, so the first answer for a new worktree can be "not one", and the .git
// write that follows corrects it.

// worktreeCacheMax bounds the verdict cache; past it the cache is dropped
// wholesale and rebuilt lazily, as the topology watcher's ignore cache is.
const worktreeCacheMax = 4096

// worktreeSkip answers "is this path inside a linked worktree of root".
//
// Concurrency: safe for concurrent use; mu guards known.
type worktreeSkip struct {
	root  string
	mu    sync.Mutex
	known map[string]bool // directory below root -> it is a linked worktree of root
	// isWorktree is paths.LinkedWorktreeOf, a seam for tests that count lookups.
	isWorktree func(root, dir string) bool
}

func newWorktreeSkip(root string) *worktreeSkip {
	return &worktreeSkip{root: root, known: map[string]bool{}, isWorktree: paths.LinkedWorktreeOf}
}

// inside reports whether path lies inside a linked worktree of root: whether a
// directory strictly between root and path is one. The root itself is never
// judged (a workspace that IS a linked worktree is watched), and nor is path
// itself, which may be a file; arrived reports on a directory that just arrived.
func (s *worktreeSkip) inside(path string) bool {
	if s == nil {
		return false
	}
	rel, ok := s.below(path)
	if !ok {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgetLocked(path)
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
// itself a linked worktree — which is then not expanded. Not cached: the
// directory's own events invalidate it anyway.
func (s *worktreeSkip) arrived(dir string) bool {
	if s == nil {
		return false
	}
	if _, ok := s.below(dir); !ok {
		return false
	}
	return s.isWorktree(s.root, dir)
}

// below is path relative to root, when it lies strictly below it.
func (s *worktreeSkip) below(path string) (string, bool) {
	rel, err := filepath.Rel(s.root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// forgetLocked drops what an event at path may have changed: the verdict for
// path itself (a directory created, removed or renamed) and, for its .git entry,
// the verdict for the directory holding it. s.mu must be held.
func (s *worktreeSkip) forgetLocked(path string) {
	delete(s.known, path)
	if filepath.Base(path) == ".git" {
		delete(s.known, filepath.Dir(path))
	}
}

// verdictLocked is dir's cached verdict, decided on a miss. s.mu must be held.
func (s *worktreeSkip) verdictLocked(dir string) bool {
	if v, ok := s.known[dir]; ok {
		return v
	}
	if len(s.known) >= worktreeCacheMax {
		clear(s.known)
	}
	v := s.isWorktree(s.root, dir)
	s.known[dir] = v
	return v
}
