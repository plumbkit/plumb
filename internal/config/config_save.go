package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// Print writes cfg as TOML to w.
func Print(cfg Config, w io.Writer) error {
	return toml.NewEncoder(w).Encode(cfg)
}

// Save persists a change to the global config file without disturbing other
// settings. It loads the current global config first so existing values are
// preserved, applies the mutation, then re-encodes the full struct. Creates
// the config file (and parent directory) if they do not yet exist.
//
// A missing config file is not an error (Load returns defaults), so first-save
// still creates one. But if the file exists and is unparseable, Load returns an
// error and Save refuses rather than overwriting the user's recoverable
// settings with defaults.
//
// The write is atomic: the encoded config is staged in a temp file in the same
// directory and renamed over the target, so a concurrent reader or a file
// watcher never observes a half-written config and a crash mid-write leaves the
// previous file intact.
//
// Known limitation: re-encoding rewrites the whole file, so any comments the
// user added by hand are lost on the first save.
func Save(apply func(*Config)) error {
	cfg, err := Load()
	if err != nil {
		return fmt.Errorf("refusing to save config: existing config is unreadable (fix it first): %w", err)
	}
	apply(&cfg)
	path := GlobalConfigPath()
	if path == "" {
		return fmt.Errorf("writing config: no config path could be resolved")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}
	return writeConfigAtomic(path, cfg)
}

// writeConfigAtomic encodes cfg and writes it atomically over path. Thin
// wrapper over writeTOMLAtomic kept for call-site clarity (global Save).
func writeConfigAtomic(path string, cfg Config) error {
	return writeTOMLAtomic(path, cfg)
}

// writeTOMLAtomic encodes v to a temp file in the target's directory, then
// renames it over path. The rename is atomic on a single filesystem. The temp
// name is dotfile-prefixed so a directory watcher filtering on the "config.toml"
// basename ignores the staging writes and reacts only to the final rename.
// Existing file permissions are preserved; a new file is created 0o644. Shared
// by the global Save (a Config struct) and the per-project sparse writers (a
// map[string]any) — see project_write.go.
func writeTOMLAtomic(path string, v any) error {
	dir := filepath.Dir(path)
	mode := os.FileMode(0o644)
	if fi, statErr := os.Stat(path); statErr == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(dir, ".config-*.toml.tmp")
	if err != nil {
		return fmt.Errorf("creating temp config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := toml.NewEncoder(tmp).Encode(v); err != nil {
		tmp.Close()
		return fmt.Errorf("encoding config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("flushing config: %w", err)
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return fmt.Errorf("setting config permissions: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming config into place: %w", err)
	}
	return nil
}

// loadGlobalRaw reads the global config file into a nested map of only the keys
// it explicitly contains — no compiled defaults, no env overlay. Returns an
// empty (non-nil) map when the file is absent, and an error when it exists but
// is unparseable (so a sparse write refuses rather than clobbering recoverable
// settings). Mirrors LoadProjectRaw for the global scope; it is the basis for
// sparse global writes that must not bake defaults or active PLUMB_* env
// overrides into the file as if they were user settings.
func loadGlobalRaw() (map[string]any, error) {
	m := map[string]any{}
	path := GlobalConfigPath()
	if path == "" {
		return m, nil
	}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := toml.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("refusing to save config: existing config is unreadable (fix it first): %w", err)
		}
	case os.IsNotExist(err):
		// absent → empty map (first save creates the file)
	default:
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// SetGlobalValue writes value at the dotted TOML key path in the global config
// file, preserving every other key already present and WITHOUT materialising
// compiled defaults or active PLUMB_* env overrides. Use it for single-setting
// persistence (the TUI theme picker) where Save's full-struct re-encode would
// otherwise write an env override or a default into the file as if the user had
// set it. The write is atomic (temp + rename), like Save.
func SetGlobalValue(path []string, value any) error {
	if len(path) == 0 {
		return fmt.Errorf("global config: empty key path")
	}
	cfgPath := GlobalConfigPath()
	if cfgPath == "" {
		return fmt.Errorf("writing config: no config path could be resolved")
	}
	m, err := loadGlobalRaw()
	if err != nil {
		return err
	}
	setNested(m, path, value)
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}
	return writeTOMLAtomic(cfgPath, m)
}

// SaveTheme persists themeName into the [ui] section of the global config file.
// It writes ONLY the ui.theme key (a sparse write), preserving every other
// setting already in the file and — crucially — never materialising defaults or
// active PLUMB_* env overrides into it. A full-struct Save would re-encode the
// resolved config (env included), so picking a theme with, say,
// PLUMB_WRITE_RATE_LIMIT set would silently persist that override; the sparse
// write avoids that.
func SaveTheme(themeName string) error {
	return SetGlobalValue([]string{"ui", "theme"}, themeName)
}
