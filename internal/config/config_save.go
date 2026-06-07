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

// SaveTheme persists themeName into the [ui] section of the global config
// file, preserving all other settings.
func SaveTheme(themeName string) error {
	return Save(func(c *Config) { c.UI.Theme = themeName })
}
