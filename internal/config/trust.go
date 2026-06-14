package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// trust.go records, per absolute workspace root, whether the user has trusted
// that workspace's project-supplied task commands (its .plumb/config.toml). The
// record lives in plumb's own data dir — never in the project — so a cloned
// repository can never mark itself trusted (the VS Code "workspace trust"
// pattern). Default-supplied and global-config task commands need no trust;
// only a command a project overrides does.
//
// Concurrency: TrustStore serialises reads and writes with a mutex; the on-disk
// file is rewritten atomically.
type TrustStore struct {
	mu   sync.Mutex
	path string
}

// NewTrustStore returns a store backed by <DataDir>/trust.json.
func NewTrustStore() *TrustStore {
	return newTrustStoreAt(filepath.Join(DataDir(), "trust.json"))
}

// newTrustStoreAt backs the store with an explicit file — the test seam.
func newTrustStoreAt(path string) *TrustStore {
	return &TrustStore{path: path}
}

func (s *TrustStore) load() (map[string]bool, error) {
	data, err := os.ReadFile(s.path)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		return map[string]bool{}, nil
	default:
		return nil, fmt.Errorf("reading trust store %s: %w", s.path, err)
	}
	m := map[string]bool{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing trust store %s: %w", s.path, err)
	}
	return m, nil
}

// IsTrusted reports whether root's project task commands are trusted. A read
// error fails closed (untrusted).
func (s *TrustStore) IsTrusted(root string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return false
	}
	return m[canonRoot(root)]
}

// SetTrusted records (trusted=true) or clears (false) trust for root, persisting
// the change atomically.
func (s *TrustStore) SetTrusted(root string, trusted bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	key := canonRoot(root)
	if trusted {
		m[key] = true
	} else {
		delete(m, key)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}
	return writeJSONAtomic(s.path, m)
}

// canonRoot returns the absolute, cleaned form of root used as the map key.
func canonRoot(root string) string {
	if abs, err := filepath.Abs(root); err == nil {
		return abs
	}
	return filepath.Clean(root)
}

// writeJSONAtomic marshals v to JSON and writes it atomically (temp file in the
// target dir + rename). Shared by the trust store and the provenance sidecar.
func writeJSONAtomic(path string, v any) error {
	dir := filepath.Dir(path)
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding json: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".plumb-*.json.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("flushing temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming into place: %w", err)
	}
	return nil
}
