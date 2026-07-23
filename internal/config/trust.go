package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// trust.go records, per absolute workspace root, whether the user has trusted
// that workspace's project-supplied task commands (its .plumb/config.toml). The
// record lives in plumb's own data dir — never in the project — so a cloned
// repository can never mark itself trusted (the VS Code "workspace trust"
// pattern). Default-supplied and global-config task commands need no trust;
// only a command a project overrides does.
//
// Trust binding: a record carries a canonical hash of the project-supplied task
// command set that was trusted (see canonicalTaskHash). The task gate compares
// the current command set's hash against the recorded one, so an agent that
// rewrites a trusted `tasks.<lang>` command after `plumb trust` cannot have the
// new command run without a re-prompt (closes the trust TOCTOU). A coarse
// Trusted flag serves the non-task surfaces that share this per-root grant
// (run_command's [[command]] allow-list, execute_shell_command's policy, and the
// xcode auto-build-server), which are gated on the bare boolean, not the task
// hash.
//
// Concurrency: TrustStore serialises reads and writes with a mutex; the on-disk
// file is rewritten atomically.
type TrustStore struct {
	mu   sync.Mutex
	path string
}

// trustRecord is the per-root on-disk trust entry. The legacy on-disk format was
// a bare bool per root; those entries are treated as untrusted and re-confirmed
// once on the next `plumb trust` (see load).
type trustRecord struct {
	// Trusted is the coarse grant covering the project's non-task execution
	// surfaces (run_command, execute_shell_command, xcode auto-build-server).
	Trusted bool `json:"trusted"`
	// TaskHash binds trust to the exact project-supplied task command set that was
	// trusted (canonical SHA-256, hex). Empty means the task gate treats the root
	// as untrusted until `plumb trust` records a hash.
	TaskHash string `json:"task_hash,omitempty"`
}

// TaskCommandSpec identifies one project-supplied task command for trust
// binding: its language, slot, and the exact command string.
type TaskCommandSpec struct {
	Lang    string
	Slot    string
	Command string
}

// NewTrustStore returns a store backed by <DataDir>/trust.json.
func NewTrustStore() *TrustStore {
	return newTrustStoreAt(filepath.Join(DataDir(), "trust.json"))
}

// newTrustStoreAt backs the store with an explicit file — the test seam.
func newTrustStoreAt(path string) *TrustStore {
	return &TrustStore{path: path}
}

// load reads the trust file into records. It tolerates the legacy `map[string]
// bool` format: a legacy boolean entry is dropped (treated as untrusted), so a
// schema migration re-confirms trust exactly once via `plumb trust`.
func (s *TrustStore) load() (map[string]trustRecord, error) {
	data, err := os.ReadFile(s.path)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		return map[string]trustRecord{}, nil
	default:
		return nil, fmt.Errorf("reading trust store %s: %w", s.path, err)
	}
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing trust store %s: %w", s.path, err)
	}
	out := make(map[string]trustRecord, len(raw))
	for k, v := range raw {
		var rec trustRecord
		if err := json.Unmarshal(v, &rec); err == nil {
			out[k] = rec
			continue
		}
		// Legacy bare-bool entry: treat as untrusted (drop it). Re-running
		// `plumb trust` re-confirms and records the new bound record.
		var b bool
		if err := json.Unmarshal(v, &b); err == nil {
			continue
		}
		return nil, fmt.Errorf("parsing trust store entry %q in %s", k, s.path)
	}
	return out, nil
}

// save persists records atomically.
func (s *TrustStore) save(m map[string]trustRecord) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}
	return writeJSONAtomic(s.path, m)
}

// IsTrusted reports whether root has a coarse trust grant. It backs the non-task
// execution surfaces (run_command, execute_shell_command, xcode) that share this
// per-root grant. A read error fails closed (untrusted). The task gate uses
// IsTrustedForTasks, not this, so a task-command change never silently re-enables
// a project command through this coarse boolean.
func (s *TrustStore) IsTrusted(root string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return false
	}
	return m[canonRoot(root)].Trusted
}

// IsTrustedForTasks reports whether root's project-supplied task commands are
// trusted AND unchanged since trust was recorded: the recorded TaskHash must
// match the canonical hash of cmds. A read error, an absent record, or a hash
// mismatch (any add/remove/modify of a task command) all fail closed.
func (s *TrustStore) IsTrustedForTasks(root string, cmds []TaskCommandSpec) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return false
	}
	rec, ok := m[canonRoot(root)]
	if !ok || rec.TaskHash == "" {
		return false
	}
	return rec.TaskHash == canonicalTaskHash(cmds)
}

// SetTrusted records (trusted=true) or clears (false) the coarse grant for root,
// persisting the change atomically. Setting preserves any existing TaskHash so a
// coarse re-grant (e.g. the TUI Commands tab) does not disturb the task binding;
// clearing removes the whole record.
func (s *TrustStore) SetTrusted(root string, trusted bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	key := canonRoot(root)
	if trusted {
		rec := m[key]
		rec.Trusted = true
		m[key] = rec
	} else {
		delete(m, key)
	}
	return s.save(m)
}

// SetTrustedForTasks grants trust for root and binds it to cmds: it records the
// canonical task-command hash and sets the coarse Trusted flag (so `plumb trust`
// grants both the task binding and the shared non-task surfaces at once).
func (s *TrustStore) SetTrustedForTasks(root string, cmds []TaskCommandSpec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	key := canonRoot(root)
	m[key] = trustRecord{Trusted: true, TaskHash: canonicalTaskHash(cmds)}
	return s.save(m)
}

// canonicalTaskHash computes a stable, ordering-independent SHA-256 (hex) of a
// task command set. Each command is rendered as its three fields
// length-prefixed and concatenated (encodeField), which is injective by
// construction — decoding does not rely on a separator byte, so no content any
// field could contain (a literal "\n", "\x1f", or digit run) can shift a field
// or record boundary and re-partition one command set into another. The
// per-command encodings are sorted and concatenated (again no separator
// needed: each encoding is self-delimiting, so record boundaries survive
// concatenation) before hashing. Two sets with the same commands in a
// different order hash identically; any add, remove, or modification of a
// command changes the hash. The empty set has a fixed, non-empty hash.
func canonicalTaskHash(cmds []TaskCommandSpec) string {
	lines := make([]string, 0, len(cmds))
	for _, c := range cmds {
		lines = append(lines, encodeField(c.Lang)+encodeField(c.Slot)+encodeField(c.Command))
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "")))
	return hex.EncodeToString(sum[:])
}

// encodeField renders s as a length-prefixed field ("<byte length>:<s>"), a
// netstring-style encoding: a reader knows exactly how many bytes belong to s
// from the prefix, so no byte s contains (including a literal ':' or a digit)
// can be misread as a boundary. This is what makes canonicalTaskHash's
// concatenation injective without needing a reserved separator byte.
func encodeField(s string) string {
	return strconv.Itoa(len(s)) + ":" + s
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
