package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// GlobalConfigPath returns the path where the global config file lives.
// Useful for diagnostics that want to report where settings come from.
func GlobalConfigPath() string {
	return configPath()
}

// CacheDir returns the path to the ephemeral plumb cache directory.
// This is for disposable state: sockets, pids, locks.
func CacheDir() string {
	return cachePath()
}

// DataDir returns the path to the persistent plumb data directory.
// This is for important history: stats.db, telemetry.
func DataDir() string {
	return dataPath()
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

func cachePath() string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "plumb")
}

func dataPath() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return cachePath() // fallback to cache if home unknown
		}
		switch runtime.GOOS {
		case "darwin":
			base = filepath.Join(home, "Library", "Application Support")
		case "windows":
			base = os.Getenv("APPDATA")
			if base == "" {
				base = filepath.Join(home, "AppData", "Roaming")
			}
		default:
			base = filepath.Join(home, ".local", "share")
		}
	}
	return filepath.Join(base, "plumb")
}

// Load reads the config file, applies env overrides, and validates the result.
// A missing config file is not an error — defaults are returned.
func Load() (Config, error) {
	cfg := cloneConfig(defaults)

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
	normaliseConfig(&cfg)

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
	if v := os.Getenv("PLUMB_LOG_FORMAT"); v != "" {
		cfg.LogFormat = v
	}
	applyEditsEnv(cfg)
	applyGitEnv(cfg)
	if v, ok := envBool("PLUMB_REFUSE_HOME_ROOTS"); ok {
		cfg.Walk.RefuseHomeRoots = v
	}
	if v, ok := envBool("PLUMB_AUTO_ATTACH"); ok {
		cfg.Workspace.AutoAttach = v
	}
	if v, ok := envBool("PLUMB_AUTO_ATTACH_PERSIST"); ok {
		cfg.Workspace.AutoAttachPersist = v
	}
	if d, ok := envDuration("PLUMB_LSP_QUERY_TIMEOUT"); ok {
		cfg.LSPQuery.Timeout = Duration{d}
	}
	normaliseConfig(cfg)
}

// applyEditsEnv overlays the [edits] environment variables onto cfg.
func applyEditsEnv(cfg *Config) {
	if v, ok := envBool("PLUMB_STRICT_EDITS"); ok {
		cfg.Edits.Strict = v
	}
	if n, ok := envNonNegInt("PLUMB_WRITE_RATE_LIMIT"); ok {
		cfg.Edits.RateLimitPerMinute = n
	}
	if n, ok := envNonNegInt("PLUMB_POST_WRITE_DIAG_MS"); ok {
		cfg.Edits.PostWriteDiagnosticsMs = n
	}
	if n, ok := envNonNegInt("PLUMB_CONCURRENT_WRITE_SKEW_MS"); ok {
		cfg.Edits.ConcurrentWriteSkewMs = n
	}
	if v, ok := envBoolNeg("PLUMB_SHOW_WRITE_DIFF"); ok {
		cfg.Edits.ShowWriteDiff = v
	}
}

// applyGitEnv overlays the [git] environment variables onto cfg.
func applyGitEnv(cfg *Config) {
	if v, ok := envBoolNeg("PLUMB_GIT_ALLOW_WRITES"); ok {
		cfg.Git.AllowWrites = v
	}
	if v, ok := envBool("PLUMB_GIT_ALLOW_DESTRUCTIVE"); ok {
		cfg.Git.AllowDestructive = v
	}
	if v, ok := envBool("PLUMB_GIT_ALLOW_PUSH"); ok {
		cfg.Git.AllowPush = v
	}
}

// envBool reads key from the environment. ok is true when the variable is
// set; v reflects whether the value is a recognised truthy string.
func envBool(key string) (v bool, ok bool) {
	s := os.Getenv(key)
	if s == "" {
		return false, false
	}
	return s == "1" || s == "true" || s == "yes", true
}

// envBoolNeg reads key from the environment and returns the logical inverse
// of recognised falsy strings ("0", "false", "no"). ok is true when set.
func envBoolNeg(key string) (v bool, ok bool) {
	s := os.Getenv(key)
	if s == "" {
		return false, false
	}
	return s != "0" && s != "false" && s != "no", true
}

// envNonNegInt reads key from the environment and parses it as a
// non-negative integer. ok is false when unset, unparseable, or negative.
func envNonNegInt(key string) (n int, ok bool) {
	s := os.Getenv(key)
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// envDuration reads key from the environment and parses it as a non-negative
// Go duration (e.g. "30s", "2m"). ok is false when unset, unparseable, or
// negative.
func envDuration(key string) (d time.Duration, ok bool) {
	s := os.Getenv(key)
	if s == "" {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, false
	}
	return d, true
}

func normaliseConfig(cfg *Config) {
	if cfg.Workspace.AutoAttachPersist {
		cfg.Workspace.AutoAttach = true
	}
	for name, lsp := range cfg.LSP {
		if expanded := expandPath(lsp.Command); expanded != lsp.Command {
			lsp.Command = expanded
			cfg.LSP[name] = lsp
		}
	}
}

// expandPath expands a leading "~/" (or bare "~") to the current user's home
// directory, then expands environment variables ($HOME, $GOPATH, etc.).
// Used so LSP command paths in config files are portable across machines.
func expandPath(s string) string {
	s = os.ExpandEnv(s)
	switch {
	case s == "~":
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	case strings.HasPrefix(s, "~/"):
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, s[2:])
		}
	}
	return s
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
	merged := cloneConfig(base)
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
	normaliseConfig(&merged)
	if err := validate(merged); err != nil {
		return base, fmt.Errorf("invalid project config: %w", err)
	}
	return merged, nil
}
