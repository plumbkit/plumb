package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// provenance.go records which project-config keys the agent-writable-config tool
// wrote, in a sidecar .plumb/config.provenance.json next to the project config.
// go-toml drops comments on a sparse rewrite, so an inline marker would not
// survive — a JSON sidecar does. `plumb config show` reads it to label
// agent-written values, and the revert path drops an entry.
//
// Concurrency: the sidecar is rewritten atomically; the caller serialises agent
// writes per workspace.

// ProvenanceEntry records who wrote a config value and when.
type ProvenanceEntry struct {
	Source    string    `json:"source"` // always "agent"
	SessionID string    `json:"session_id,omitempty"`
	Client    string    `json:"client,omitempty"`
	Timestamp time.Time `json:"timestamp"`
	Previous  *string   `json:"previous,omitempty"` // prior project value, for revert display
}

// Provenance maps a dotted config key to who last wrote it.
type Provenance map[string]ProvenanceEntry

func provenancePath(workspace string) string {
	if workspace == "" {
		return ""
	}
	return filepath.Join(workspace, ".plumb", "config.provenance.json")
}

// LoadProvenance reads the sidecar. A missing file is an empty (non-nil) map.
func LoadProvenance(workspace string) (Provenance, error) {
	path := provenancePath(workspace)
	if path == "" {
		return Provenance{}, nil
	}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		return Provenance{}, nil
	default:
		return nil, fmt.Errorf("reading provenance %s: %w", path, err)
	}
	m := Provenance{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing provenance %s: %w", path, err)
	}
	return m, nil
}

// RecordAgentWrite stamps key with entry and persists the sidecar atomically.
func RecordAgentWrite(workspace, key string, entry ProvenanceEntry) error {
	path := provenancePath(workspace)
	if path == "" {
		return fmt.Errorf("provenance: no workspace path")
	}
	m, err := LoadProvenance(workspace)
	if err != nil {
		return err
	}
	m[key] = entry
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating .plumb dir: %w", err)
	}
	ensureProvenanceGitignore(workspace)
	return writeJSONAtomic(path, m)
}

// ensureProvenanceGitignore makes sure <ws>/.plumb/.gitignore excludes the
// provenance sidecar — local audit metadata (session ids, timestamps) that must
// never be committed, even in a workspace that deliberately tracks .plumb/.
// Mirrors topology's ensureGitignore. Best-effort; failure is non-fatal.
func ensureProvenanceGitignore(workspace string) {
	const entry = "config.provenance.json"
	const header = "# plumb agent-config provenance (local audit metadata; do not commit)"
	path := filepath.Join(workspace, ".plumb", ".gitignore")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return
	}
	if bytes.Contains(existing, []byte(entry)) {
		return
	}
	var b bytes.Buffer
	b.Write(existing)
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		b.WriteByte('\n')
	}
	b.WriteString(header + "\n" + entry + "\n")
	_ = os.WriteFile(path, b.Bytes(), 0o644) //nolint:gosec // G306: .gitignore is a normal repo file
}

// DropProvenance removes key from the sidecar (the revert path), removing the
// file when it becomes empty.
func DropProvenance(workspace, key string) error {
	path := provenancePath(workspace)
	if path == "" {
		return nil
	}
	m, err := LoadProvenance(workspace)
	if err != nil {
		return err
	}
	if _, ok := m[key]; !ok {
		return nil
	}
	delete(m, key)
	if len(m) == 0 {
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			return fmt.Errorf("removing empty provenance: %w", rmErr)
		}
		return nil
	}
	return writeJSONAtomic(path, m)
}
