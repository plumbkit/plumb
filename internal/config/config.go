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
	"maps"
	"os"
	"path/filepath"
	"runtime"
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
	return []byte(d.String()), nil
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

// WorkspaceConfig controls how the daemon identifies the workspace root for
// sessions that don't match any recognised project marker.
type WorkspaceConfig struct {
	// AutoAttach enables the synthetic-root fallback. When true and a tool
	// call's seed path does not match any .plumb/, go.mod, or other project
	// marker, the daemon walks up to the nearest .git/ directory (or uses the
	// seed directory itself) as the workspace root. Stats, project config, and
	// TUI attribution all work normally; LSP is unavailable. Default false.
	AutoAttach bool `toml:"auto_attach"`
	// AutoAttachPersist, when true, causes the daemon to create a .plumb/
	// directory at the synthetic root on first attach. On subsequent sessions
	// the directory resolves via the standard marker path and auto-attach is
	// no longer needed. Implies AutoAttach. Default false.
	AutoAttachPersist bool `toml:"auto_attach_persist"`
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
	// PostWriteDiagnosticsMs is how long (in milliseconds) write/edit tools
	// wait for the LSP server to re-publish diagnostics after a successful
	// write. 0 disables the wait entirely. Defaults to 300.
	PostWriteDiagnosticsMs int `toml:"post_write_diagnostics_ms"`
	// ConcurrentWriteSkewMs is the clock-skew allowance (in milliseconds)
	// used by edit_file's concurrent-write detector. After a rename, the
	// file's mtime must be newer than tempWrittenAt+skew to trigger a retry.
	// Increase on slow filesystems (network mounts, FUSE). Defaults to 100.
	ConcurrentWriteSkewMs int `toml:"concurrent_write_skew_ms"`
	// ShowWriteDiff controls whether edit_file and write_file append a unified
	// diff of the change to their response. Defaults to true. Set to false
	// (or PLUMB_SHOW_WRITE_DIFF=0) for implicit-verification mode where only
	// path, size, and mtime metadata are returned — useful when tokens matter
	// more than inline confirmation.
	ShowWriteDiff bool `toml:"show_write_diff"`
}

// TopologyConfig controls the persistent semantic index.
// All fields can be overridden per-project via <workspace>/.plumb/config.toml.
type TopologyConfig struct {
	// Enabled turns topology indexing on. Default false (opt-in).
	Enabled bool `toml:"enabled"`
	// ResyncOnAttach triggers a full resync whenever the workspace attaches.
	ResyncOnAttach bool `toml:"resync_on_attach"`
	// ExcludePatterns is an optional list of path glob patterns to skip during indexing.
	ExcludePatterns []string `toml:"exclude_patterns"`
	// MaxFileSizeBytes caps the file size considered for extraction.
	// Default 512 KiB. 0 means use the default.
	MaxFileSizeBytes int64 `toml:"max_file_size_bytes"`
	// ResyncIntervalMinutes is the interval between full resyncs. 0 disables periodic resync.
	ResyncIntervalMinutes int `toml:"resync_interval_minutes"`
}

// QualityConfig controls post-write offline code-quality analysis.
// All fields can be overridden per-project via <workspace>/.plumb/config.toml.
type QualityConfig struct {
	// Enabled turns quality analysis on. Default false (opt-in until proven in use).
	Enabled bool `toml:"enabled"`
	// Mode is "background" (default) or "sync".
	//   background — enqueue files; findings available on the next request.
	//   sync       — block up to TimeoutMs and append findings inline.
	Mode string `toml:"mode"`
	// Analysers lists which analysers to run. Default ["golangci-lint"].
	Analysers []string `toml:"analysers"`
	// TimeoutMs caps each analyser run in milliseconds. Default 2000.
	TimeoutMs int `toml:"timeout_ms"`
	// MaxFindingsPerFile caps findings appended per file to keep responses
	// bounded. Default 5.
	MaxFindingsPerFile int `toml:"max_findings_per_file"`
}

// Config is the resolved configuration for a plumb process.
// Concurrency: read-only after Load returns.
type Config struct {
	LogLevel  string               `toml:"log_level"`
	LogFormat string               `toml:"log_format"`
	LogFile   string               `toml:"log_file"`
	Cache     CacheConfig          `toml:"cache"`
	Edits     EditsConfig          `toml:"edits"`
	Walk      WalkConfig           `toml:"walk"`
	Workspace WorkspaceConfig      `toml:"workspace"`
	Quality   QualityConfig        `toml:"quality"`
	Topology  TopologyConfig       `toml:"topology"`
	LSP       map[string]LSPConfig `toml:"lsp"`
}

var defaults = Config{
	LogLevel:  "info",
	LogFormat: "text",
	Cache: CacheConfig{
		TTL:     Duration{5 * time.Minute},
		MaxSize: 1000,
	},
	Edits: EditsConfig{
		Strict:                 false,
		RateLimitPerMinute:     120,
		PostWriteDiagnosticsMs: 300,
		ConcurrentWriteSkewMs:  100,
		ShowWriteDiff:          true,
	},
	Walk: WalkConfig{
		RefuseHomeRoots: true,
	},
	Quality: QualityConfig{
		Enabled:            false,
		Mode:               "background",
		Analysers:          []string{"golangci-lint"},
		TimeoutMs:          2000,
		MaxFindingsPerFile: 5,
	},
	Topology: TopologyConfig{
		Enabled:          false,
		MaxFileSizeBytes: 512 * 1024,
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
		"java": {
			Command:     "jdtls",
			Args:        []string{},
			RootMarkers: []string{"pom.xml", "build.gradle", "build.gradle.kts", ".classpath"},
			Enabled:     false,
		},
	},
}

// Defaults returns a copy of the compiled-in defaults. Useful for CLI tools
// that want to compare what's in the resolved config against the baseline.
func Defaults() Config {
	return cloneConfig(defaults)
}

func cloneConfig(cfg Config) Config {
	out := cfg
	if cfg.Topology.ExcludePatterns != nil {
		out.Topology.ExcludePatterns = append([]string(nil), cfg.Topology.ExcludePatterns...)
	}
	if cfg.LSP != nil {
		out.LSP = make(map[string]LSPConfig, len(cfg.LSP))
		for name, lspCfg := range cfg.LSP {
			out.LSP[name] = cloneLSPConfig(lspCfg)
		}
	}
	return out
}

func cloneLSPConfig(cfg LSPConfig) LSPConfig {
	out := cfg
	if cfg.Args != nil {
		out.Args = append([]string(nil), cfg.Args...)
	}
	if cfg.RootMarkers != nil {
		out.RootMarkers = append([]string(nil), cfg.RootMarkers...)
	}
	if cfg.Env != nil {
		out.Env = make(map[string]string, len(cfg.Env))
		maps.Copy(out.Env, cfg.Env)
	}
	return out
}

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
	if v, ok := envBool("PLUMB_REFUSE_HOME_ROOTS"); ok {
		cfg.Walk.RefuseHomeRoots = v
	}
	if v, ok := envBool("PLUMB_AUTO_ATTACH"); ok {
		cfg.Workspace.AutoAttach = v
	}
	if v, ok := envBool("PLUMB_AUTO_ATTACH_PERSIST"); ok {
		cfg.Workspace.AutoAttachPersist = v
	}
	normaliseConfig(cfg)
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

func normaliseConfig(cfg *Config) {
	if cfg.Workspace.AutoAttachPersist {
		cfg.Workspace.AutoAttach = true
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

func validate(cfg Config) error {
	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level must be one of debug, info, warn, error; got %q", cfg.LogLevel)
	}
	switch cfg.LogFormat {
	case "text", "json":
	default:
		return fmt.Errorf("log_format must be one of text, json; got %q", cfg.LogFormat)
	}
	if cfg.Cache.MaxSize < 0 {
		return fmt.Errorf("cache.max_size must be non-negative")
	}
	if cfg.Edits.RateLimitPerMinute < 0 {
		return fmt.Errorf("edits.rate_limit_per_minute must be non-negative (0 disables)")
	}
	if cfg.Edits.PostWriteDiagnosticsMs < 0 {
		return fmt.Errorf("edits.post_write_diagnostics_ms must be non-negative (0 disables)")
	}
	if cfg.Edits.ConcurrentWriteSkewMs < 0 {
		return fmt.Errorf("edits.concurrent_write_skew_ms must be non-negative")
	}
	switch cfg.Quality.Mode {
	case "", "background", "sync":
	default:
		return fmt.Errorf("quality.mode must be \"background\" or \"sync\"; got %q", cfg.Quality.Mode)
	}
	if cfg.Quality.TimeoutMs < 0 {
		return fmt.Errorf("quality.timeout_ms must be non-negative")
	}
	if cfg.Quality.MaxFindingsPerFile < 0 {
		return fmt.Errorf("quality.max_findings_per_file must be non-negative")
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
