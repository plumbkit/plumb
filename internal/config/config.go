// Package config loads and validates plumb's TOML configuration.
//
// Precedence (lowest → highest):
//
//  1. Compiled defaults
//  2. Global config (~/.config/plumb/config.toml, honouring XDG_CONFIG_HOME)
//  3. Project-local config (<workspace>/.plumb/config.toml), loaded via LoadProject
//     once the connection's workspace is resolved
//  4. Environment variables
//  5. CLI flags
//
// Each layer overwrites only the fields it sets — project-local config does
// not have to repeat global settings to keep them.
package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Duration wraps time.Duration so go-toml can unmarshal human-friendly strings
// like "5m" or "30s" from the config file.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	dur, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = dur
	return nil
}

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(d.Duration.String()), nil
}

// LSPConfig holds per-language-server settings.
// Concurrency: read-only after Load returns.
type LSPConfig struct {
	Command     string            `toml:"command"`
	Args        []string          `toml:"args"`
	RootMarkers []string          `toml:"root_markers"`
	Env         map[string]string `toml:"env"`
	Enabled     bool              `toml:"enabled"`
}

// CacheConfig controls the in-memory session cache.
type CacheConfig struct {
	TTL     Duration `toml:"ttl"`
	MaxSize int      `toml:"max_size"`
}

// WalkConfig controls filesystem traversal safety. On macOS, walking $HOME
// or one of its protected subdirectories (Desktop, Documents, Downloads,
// Pictures, Music, Movies, Public, iCloud Drive) triggers a TCC consent
// prompt attributed to the plumb binary. RefuseHomeRoots blocks those walks
// at the root level so callers handing plumb an unexpected root (e.g. an
// MCP client returning $HOME from roots/list) don't surface spurious prompts.
//
// Subpaths inside a protected directory are NOT refused — a real project
// at ~/Documents/MyProject is still walked. Only walks rooted exactly at a
// protected directory are refused.
//
// This setting is a no-op on non-Darwin platforms.
type WalkConfig struct {
	RefuseHomeRoots bool `toml:"refuse_home_roots"`
}

// EditsConfig controls safety behaviour for write/edit tools. Both fields
// can be set globally (~/.config/plumb/config.toml) and overridden per
// project (<workspace>/.plumb/config.toml). Environment variables
// (PLUMB_STRICT_EDITS, PLUMB_WRITE_RATE_LIMIT) override both.
type EditsConfig struct {
	// Strict: when true, edit_file requires every target to have been read
	// via read_file in this daemon's lifetime AND for the file's current
	// mtime to match what read_file observed. Defaults to false.
	Strict bool `toml:"strict"`
	// RateLimitPerMinute caps how many write operations (write_file,
	// edit_file, delete_file, rename_file, transaction_apply per-op) a
	// session may issue per minute. 0 disables limiting. Defaults to 120.
	RateLimitPerMinute int `toml:"rate_limit_per_minute"`
}

// Config is the resolved configuration for a plumb process.
// Concurrency: read-only after Load returns.
type Config struct {
	LogLevel string               `toml:"log_level"`
	LogFile  string               `toml:"log_file"`
	Cache    CacheConfig          `toml:"cache"`
	Edits    EditsConfig          `toml:"edits"`
	Walk     WalkConfig           `toml:"walk"`
	LSP      map[string]LSPConfig `toml:"lsp"`
}

var defaults = Config{
	LogLevel: "info",
	Cache: CacheConfig{
		TTL:     Duration{5 * time.Minute},
		MaxSize: 1000,
	},
	Edits: EditsConfig{
		Strict:             false,
		RateLimitPerMinute: 120,
	},
	Walk: WalkConfig{
		RefuseHomeRoots: true,
	},
	LSP: map[string]LSPConfig{
		"go": {
			Command:     "gopls",
			Args:        []string{},
			RootMarkers: []string{"go.mod"},
			Enabled:     true,
		},
		"python": {
			Command:     "pyright-langserver",
			Args:        []string{"--stdio"},
			RootMarkers: []string{"pyproject.toml", "setup.py", "pyrightconfig.json"},
			Enabled:     false,
		},
	},
}

// Defaults returns a copy of the compiled-in defaults. Useful for CLI tools
// that want to compare what's in the resolved config against the baseline.
func Defaults() Config {
	return defaults
}

// GlobalConfigPath returns the path where the global config file lives.
// Useful for diagnostics that want to report where settings come from.
func GlobalConfigPath() string {
	return configPath()
}

func configPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "plumb", "config.toml")
}

// Load reads the config file, applies env overrides, and validates the result.
// A missing config file is not an error — defaults are returned.
func Load() (Config, error) {
	cfg := defaults

	path := configPath()
	if path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			if err := toml.Unmarshal(data, &cfg); err != nil {
				return Config{}, fmt.Errorf("parsing config %s: %w", path, err)
			}
		} else if !os.IsNotExist(err) {
			return Config{}, fmt.Errorf("reading config %s: %w", path, err)
		}
	}

	applyEnv(&cfg)

	if err := validate(cfg); err != nil {
		return Config{}, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

// applyEnv overlays environment variables onto cfg.
func applyEnv(cfg *Config) {
	if v := os.Getenv("PLUMB_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("PLUMB_LOG_FILE"); v != "" {
		cfg.LogFile = v
	}
	if v := os.Getenv("PLUMB_STRICT_EDITS"); v != "" {
		cfg.Edits.Strict = v == "1" || v == "true" || v == "yes"
	}
	if v := os.Getenv("PLUMB_WRITE_RATE_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.Edits.RateLimitPerMinute = n
		}
	}
	if v := os.Getenv("PLUMB_REFUSE_HOME_ROOTS"); v != "" {
		cfg.Walk.RefuseHomeRoots = v == "1" || v == "true" || v == "yes"
	}
}

// ProjectConfigPath returns the conventional location of a workspace's
// plumb-local config: <workspace>/.plumb/config.toml.
func ProjectConfigPath(workspace string) string {
	if workspace == "" {
		return ""
	}
	return filepath.Join(workspace, ".plumb", "config.toml")
}

// LoadProject reads <workspace>/.plumb/config.toml and merges it onto base.
// Missing file is not an error; base is returned unchanged. Environment
// variable overrides are re-applied so they remain the highest-priority
// layer. Validation is performed after the merge.
//
// Call this once per connection, after the workspace has been resolved.
// The result is what tools should consult for per-project settings (strict
// mode, rate limit).
func LoadProject(base Config, workspace string) (Config, error) {
	merged := base
	path := ProjectConfigPath(workspace)
	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := toml.Unmarshal(data, &merged); err != nil {
				return base, fmt.Errorf("parsing project config %s: %w", path, err)
			}
		case os.IsNotExist(err):
			// no project config — fall through, env still applied
		default:
			return base, fmt.Errorf("reading project config %s: %w", path, err)
		}
	}
	applyEnv(&merged)
	if err := validate(merged); err != nil {
		return base, fmt.Errorf("invalid project config: %w", err)
	}
	return merged, nil
}

func validate(cfg Config) error {
	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level must be one of debug, info, warn, error; got %q", cfg.LogLevel)
	}
	if cfg.Cache.MaxSize < 0 {
		return fmt.Errorf("cache.max_size must be non-negative")
	}
	if cfg.Edits.RateLimitPerMinute < 0 {
		return fmt.Errorf("edits.rate_limit_per_minute must be non-negative (0 disables)")
	}
	for name, lsp := range cfg.LSP {
		if lsp.Enabled && lsp.Command == "" {
			return fmt.Errorf("lsp.%s.command must be set when enabled", name)
		}
	}
	return nil
}

// Print writes cfg as TOML to w.
func Print(cfg Config, w io.Writer) error {
	return toml.NewEncoder(w).Encode(cfg)
}
