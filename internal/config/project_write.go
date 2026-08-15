package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
// Keys are matched case-insensitively (lookupNested → foldLookup), mirroring
// how go-toml/v2 binds a TOML key to a struct field, so a project supplying
// `[[COMMAND]]` or `[TASKS.go]` reports present under the lowercase path —
// the exact-match miss here was the mechanism of two arbitrary-code-execution
// bypasses (run_command, run_task), closed by deriving provenance from the
// trust spec instead (issue #319 folded the lookup itself; the spec remains
// the provenance source).
//
// STILL never use this to decide whether a security gate applies: the trust
// spec (ProjectPolicyStatus.Asked, ProjectTaskCommands) is computed from the
// same bytes as the loaded config at apply time, so it cannot disagree with
// what actually loaded — a presence check re-reads the file and can. It
// remains the right tool for the TUI and web settings screens' "overridden
// vs inherited" annotation.
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
		return errors.New("project config: empty key path")
	}
	cfgPath := ProjectConfigPath(workspace)
	if cfgPath == "" {
		return errors.New("project config: no workspace path")
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
		return errors.New("project config: no workspace path")
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

// foldLookup returns the key in m that matches want case-insensitively — the
// way go-toml/v2 binds a TOML key to a struct field, so `TASKS` and `tasks`
// are the same setting — preferring the exact spelling when both appear.
// ok is false when no fold variant exists.
func foldLookup(m map[string]any, want string) (key string, ok bool) {
	if _, ok := m[want]; ok {
		return want, true
	}
	for k := range m {
		if strings.EqualFold(k, want) {
			return k, true
		}
	}
	return "", false
}

// foldDelete removes every key in m that matches want case-insensitively. The
// raw map can hold two fold variants of one setting as distinct keys (TOML
// keys are case-sensitive), and both decode into the same merged value — so
// "unset this setting" must remove both or lookup would still find it.
func foldDelete(m map[string]any, want string) {
	delete(m, want)
	for k := range m {
		if strings.EqualFold(k, want) {
			delete(m, k)
		}
	}
}

// lookupNested reports whether path resolves to a present leaf in m. Keys are
// matched case-insensitively, mirroring how go-toml/v2 decodes the same file
// into the typed Config: a fold-variant spelling is present in the effective
// config, so it must be present here too.
func lookupNested(m map[string]any, path []string) bool {
	for _, k := range path[:len(path)-1] {
		key, ok := foldLookup(m, k)
		if !ok {
			return false
		}
		next, ok := m[key].(map[string]any)
		if !ok {
			return false
		}
		m = next
	}
	_, ok := foldLookup(m, path[len(path)-1])
	return ok
}

// setNested sets value at path within m, creating intermediate tables. A
// fold-variant spelling of any path segment is written THROUGH (the existing
// key is updated in place) rather than duplicated — writing `git.allow_writes`
// into a file that says [GIT] must not leave two tables that both decode into
// Config.Git.
func setNested(m map[string]any, path []string, value any) {
	for _, k := range path[:len(path)-1] {
		key, ok := foldLookup(m, k)
		if !ok {
			key = k
		}
		next, ok := m[key].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[key] = next
		}
		m = next
	}
	key, ok := foldLookup(m, path[len(path)-1])
	if !ok {
		key = path[len(path)-1]
	}
	m[key] = value
}

// deleteNested removes path from m and prunes any table it leaves empty. Keys
// are matched case-insensitively, so unsetting a lowercase path removes a
// fold-variant spelling of the same setting.
func deleteNested(m map[string]any, path []string) {
	if len(path) == 1 {
		foldDelete(m, path[0])
		return
	}
	key, ok := foldLookup(m, path[0])
	if !ok {
		return
	}
	child, ok := m[key].(map[string]any)
	if !ok {
		return
	}
	deleteNested(child, path[1:])
	if len(child) == 0 {
		delete(m, key)
	}
}
