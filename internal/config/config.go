// Package config loads and validates plumb's TOML configuration.
// Precedence: compiled defaults → config file → environment variables → CLI flags.
package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
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

// Config is the resolved configuration for a plumb process.
// Concurrency: read-only after Load returns.
type Config struct {
	LogLevel string               `toml:"log_level"`
	LogFile  string               `toml:"log_file"`
	Cache    CacheConfig          `toml:"cache"`
	LSP      map[string]LSPConfig `toml:"lsp"`
}

var defaults = Config{
	LogLevel: "info",
	Cache: CacheConfig{
		TTL:     Duration{5 * time.Minute},
		MaxSize: 1000,
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
