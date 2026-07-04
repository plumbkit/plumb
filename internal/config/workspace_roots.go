package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// workspace_roots.go records, per absolute workspace root, the extra
// filesystem roots a user has manually granted that workspace — extra_roots
// (read-write) and read_roots (read-only) that widen the path-access allowlist
// beyond the workspace itself.
//
// Unlike [workspace] extra_roots / read_roots in a project's .plumb/config.toml
// (which LoadProject deliberately forces back to the global base — a cloned repo
// is untrusted and must not widen its own access on attach), these grants live
// in plumb's own data dir, keyed by the canonical workspace root, and are only
// ever written through a trusted surface (the TUI Settings screen / the CLI). A
// cloned repository can neither write here nor change a granted path after the
// fact, so the grant is exactly the folders the user typed — the VS Code
// "workspace trust" pattern, the same one TrustStore applies to task commands.
//
// Concurrency: WorkspaceRootsStore serialises reads and writes with a mutex; the
// on-disk file is rewritten atomically (shared writeJSONAtomic).
type WorkspaceRootsStore struct {
	mu   sync.Mutex
	path string
}

// WorkspaceRoots is the grant recorded for one workspace root: additive
// read-write and read-only roots, additive to the global config roots.
type WorkspaceRoots struct {
	ExtraRoots []string `json:"extra_roots,omitempty"`
	ReadRoots  []string `json:"read_roots,omitempty"`
}

// NewWorkspaceRootsStore returns a store backed by <DataDir>/workspace_roots.json.
func NewWorkspaceRootsStore() *WorkspaceRootsStore {
	return newWorkspaceRootsStoreAt(filepath.Join(DataDir(), "workspace_roots.json"))
}

// newWorkspaceRootsStoreAt backs the store with an explicit file — the test seam.
func newWorkspaceRootsStoreAt(path string) *WorkspaceRootsStore {
	return &WorkspaceRootsStore{path: path}
}

func (s *WorkspaceRootsStore) load() (map[string]WorkspaceRoots, error) {
	data, err := os.ReadFile(s.path)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		return map[string]WorkspaceRoots{}, nil
	default:
		return nil, fmt.Errorf("reading workspace roots store %s: %w", s.path, err)
	}
	m := map[string]WorkspaceRoots{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing workspace roots store %s: %w", s.path, err)
	}
	return m, nil
}

// Get returns the roots granted to root. A read error fails closed (empty
// grant), so a corrupt or unreadable store never widens access.
func (s *WorkspaceRootsStore) Get(root string) WorkspaceRoots {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return WorkspaceRoots{}
	}
	return m[canonRoot(root)]
}

// SetExtraRoots records root's read-write extra roots, persisting atomically. An
// empty (or nil) list clears them; a workspace left with no roots at all is
// pruned from the store entirely.
func (s *WorkspaceRootsStore) SetExtraRoots(root string, roots []string) error {
	return s.update(root, func(wr *WorkspaceRoots) { wr.ExtraRoots = normaliseRootList(roots) })
}

// SetReadRoots records root's read-only roots, persisting atomically. An empty
// (or nil) list clears them; a workspace left with no roots is pruned.
func (s *WorkspaceRootsStore) SetReadRoots(root string, roots []string) error {
	return s.update(root, func(wr *WorkspaceRoots) { wr.ReadRoots = normaliseRootList(roots) })
}

// update applies mutate to root's entry under the lock and writes the store,
// pruning an entry left with no roots so the file never accumulates empties.
func (s *WorkspaceRootsStore) update(root string, mutate func(*WorkspaceRoots)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	key := canonRoot(root)
	wr := m[key]
	mutate(&wr)
	if len(wr.ExtraRoots) == 0 && len(wr.ReadRoots) == 0 {
		delete(m, key)
	} else {
		m[key] = wr
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}
	return writeJSONAtomic(s.path, m)
}

// normaliseRootList trims blanks and drops empty entries, returning nil for an
// all-empty input so a cleared field marshals away (omitempty) and the entry can
// be pruned.
func normaliseRootList(roots []string) []string {
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		if r != "" {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
