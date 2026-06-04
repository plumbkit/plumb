package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// project_write.go implements SPARSE writes to a workspace's project config
// (<workspace>/.plumb/config.toml). Unlike Save (which re-encodes the whole
// resolved Config struct), these helpers touch only the single key the user
// changed, so a per-project override never silently shadows a global value the
// user did not set: a key absent from the file falls through to global/default
// (the "inherit" state), a key present is an explicit override.

// LoadProjectRaw reads the project config into a nested map of only the keys the
// project explicitly sets. Returns an empty (non-nil) map when the file is
// absent. This is the source of truth for the TUI's "overridden vs inherited"
// distinction — drive the inherit annotation off whether a key is present here.
func LoadProjectRaw(workspace string) (map[string]any, error) {
	m := map[string]any{}
	path := ProjectConfigPath(workspace)
	if path == "" {
		return m, nil
	}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := toml.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("parsing project config %s: %w", path, err)
		}
	case os.IsNotExist(err):
		// absent → empty map (no overrides)
	default:
		return nil, fmt.Errorf("reading project config %s: %w", path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// ProjectValuePresent reports whether the dotted key path is explicitly set in
// the workspace's project config (i.e. it is an override, not inherited).
func ProjectValuePresent(workspace string, path []string) (bool, error) {
	m, err := LoadProjectRaw(workspace)
	if err != nil {
		return false, err
	}
	return lookupNested(m, path), nil
}

// SetProjectValue writes value at the dotted TOML key path in the project
// config, creating <workspace>/.plumb/ and config.toml on first use. Only the
// touched key is written — the file stays sparse.
func SetProjectValue(workspace string, path []string, value any) error {
	if len(path) == 0 {
		return fmt.Errorf("project config: empty key path")
	}
	cfgPath := ProjectConfigPath(workspace)
	if cfgPath == "" {
		return fmt.Errorf("project config: no workspace path")
	}
	m, err := LoadProjectRaw(workspace)
	if err != nil {
		return err
	}
	setNested(m, path, value)
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return fmt.Errorf("creating .plumb dir: %w", err)
	}
	return writeTOMLAtomic(cfgPath, m)
}

// UnsetProjectValue removes the dotted key path from the project config (the
// "inherit" state — the key falls through to global/default). Tables it leaves
// empty are pruned; when the whole file becomes empty it is removed.
func UnsetProjectValue(workspace string, path []string) error {
	if len(path) == 0 {
		return nil
	}
	cfgPath := ProjectConfigPath(workspace)
	if cfgPath == "" {
		return fmt.Errorf("project config: no workspace path")
	}
	m, err := LoadProjectRaw(workspace)
	if err != nil {
		return err
	}
	deleteNested(m, path)
	if len(m) == 0 {
		if rmErr := os.Remove(cfgPath); rmErr != nil && !os.IsNotExist(rmErr) {
			return fmt.Errorf("removing empty project config: %w", rmErr)
		}
		return nil
	}
	return writeTOMLAtomic(cfgPath, m)
}

// lookupNested reports whether path resolves to a present leaf in m.
func lookupNested(m map[string]any, path []string) bool {
	for _, k := range path[:len(path)-1] {
		next, ok := m[k].(map[string]any)
		if !ok {
			return false
		}
		m = next
	}
	_, ok := m[path[len(path)-1]]
	return ok
}

// setNested sets value at path within m, creating intermediate tables.
func setNested(m map[string]any, path []string, value any) {
	for _, k := range path[:len(path)-1] {
		next, ok := m[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[k] = next
		}
		m = next
	}
	m[path[len(path)-1]] = value
}

// deleteNested removes path from m and prunes any table it leaves empty.
func deleteNested(m map[string]any, path []string) {
	if len(path) == 1 {
		delete(m, path[0])
		return
	}
	child, ok := m[path[0]].(map[string]any)
	if !ok {
		return
	}
	deleteNested(child, path[1:])
	if len(child) == 0 {
		delete(m, path[0])
	}
}
