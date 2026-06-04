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
	"strings"
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

// LSPQueryConfig bounds LSP tool operations so a slow, indexing, or wedged
// language server cannot hang a request until the MCP client's own timeout
// fires. Global only — there is no per-project override.
// Concurrency: read-only after Load returns.
type LSPQueryConfig struct {
	// Timeout caps a single LSP tool operation (query or edit) when the
	// caller's context carries no deadline. 0 disables the cap. Default 30s.
	Timeout Duration `toml:"timeout"`
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
	// ExtraRoots lists additional directories the session's filesystem tools may
	// read AND write, beyond the detected workspace. Human-authored (project or
	// global config), additive to the workspace, never replacing it. Paths may
	// use $VAR / ${VAR} (expanded with the daemon's environment). Default empty.
	ExtraRoots []string `toml:"extra_roots"`
	// ReadRoots lists additional directories the session's read/search tools may
	// read (never write), beyond the detected workspace — e.g. a vendored
	// dependency tree or a shared library checkout. Additive; $VAR expansion as
	// for ExtraRoots. Default empty.
	ReadRoots []string `toml:"read_roots"`
	// AllowDependencyReads, when true, lets read/search tools reach the language
	// toolchain's standard dependency locations read-only (for Go: the module
	// cache `go env GOMODCACHE` and `GOROOT`), so an agent can inspect a
	// dependency's source without falling back to the shell. Read-only by
	// construction — writes there are always refused. Default true.
	AllowDependencyReads bool `toml:"allow_dependency_reads"`
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

// GitConfig controls the unified git tool's tiered allowlist. Read-only
// subcommands always run. Write, destructive, and network tiers are gated by
// these flags so the same tool can be flexible on trusted workspaces and
// locked down elsewhere. All fields can be overridden per-project via
// <workspace>/.plumb/config.toml and by environment variables.
//
// Concurrency: read-only after Load returns.
type GitConfig struct {
	// AllowWrites gates the safe-write tier (add, commit, switch, branch
	// create, tag create, stash push/pop). Default true.
	AllowWrites bool `toml:"allow_writes"`
	// AllowDestructive gates the destructive tier (reset, clean, checkout,
	// restore, rebase, revert, branch/tag delete, stash drop/clear). Each call
	// also requires confirm:true. Default false.
	AllowDestructive bool `toml:"allow_destructive"`
	// AllowPush gates the network tier (push, fetch, pull). Each call also
	// requires confirm:true. Default false.
	AllowPush bool `toml:"allow_push"`
	// ProtectedBranches are branch names that may never be force-pushed, even
	// when AllowPush is true and confirm is set. Default ["main", "master"].
	ProtectedBranches []string `toml:"protected_branches"`
}

// TopologyConfig controls the persistent semantic index.
// All fields can be overridden per-project via <workspace>/.plumb/config.toml.
type TopologyConfig struct {
	// Enabled turns topology indexing on. Default true; opt out per-project or
	// globally with `enabled = false`. When on, the index lives at
	// <workspace>/.plumb/topology.db (auto-gitignored), created on first attach.
	Enabled bool `toml:"enabled"`
	// ResyncOnAttach triggers a full resync whenever the workspace attaches.
	ResyncOnAttach bool `toml:"resync_on_attach"`
	// ExcludePatterns is an optional list of path glob patterns to skip during indexing.
	ExcludePatterns []string `toml:"exclude_patterns"`
	// MaxFileSizeBytes caps the file size considered for extraction.
	// Default 512 KiB. 0 means use the default.
	MaxFileSizeBytes int64 `toml:"max_file_size_bytes"`
	// ResyncBatch is the number of files a full resync extracts before pausing
	// for ResyncPauseMs, so the indexer yields CPU to live tool calls on a large
	// workspace. Only the full resync walk is paced; write-triggered upserts are
	// never delayed. 0 disables pacing. Default 100.
	ResyncBatch int `toml:"resync_batch"`
	// ResyncPauseMs is the pause (milliseconds) inserted after each ResyncBatch
	// files during a full resync. 0 disables pacing. Default 25.
	ResyncPauseMs int `toml:"resync_pause_ms"`
	// ResyncIntervalMinutes is the interval between full resyncs. 0 disables periodic resync. Default 60.
	ResyncIntervalMinutes int `toml:"resync_interval_minutes"`
	// Watch enables OS-level file-system watching: any change to a source file —
	// by this agent, another agent, or an external editor — is re-indexed at the
	// moment it happens, instead of waiting for a periodic resync. Default true.
	// When the watcher is live the periodic resync is suppressed (freshness is
	// event-driven, with a full resync still triggered on a dropped/overflow
	// signal); when the watcher is disabled or unavailable, ResyncIntervalMinutes
	// remains the fallback.
	Watch bool `toml:"watch"`
}

// UIConfig controls presentation settings stored in the global config only.
// These are TUI-layer preferences; project-local overrides are not supported.
type UIConfig struct {
	// Theme is the key of the active colour theme in tui.AvailableThemes.
	// Default "nordico". Persisted by the TUI theme picker via SaveTheme.
	Theme string `toml:"theme"`
	// PathStyle controls how workspace folder paths are abbreviated in the
	// Sessions sidebar. "compact" (default) shows the tilde-home prefix, the
	// first letter of each intermediate directory component, and the full last
	// component — e.g. ~/P/e/o/cve-explorer. "truncate-middle" trims the left
	// side of the path and keeps the tail. "full" shows the full tilde-home
	// path and only falls back to ellipsis+last when still over the column width.
	PathStyle string `toml:"path_style"`
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

// SessionConfig controls session lifecycle: idle detection and eviction of
// connections whose agents have silently disconnected.
// Concurrency: read-only after Load returns.
type SessionConfig struct {
	// IdleThresholdMinutes is how long after the last tool call a session is
	// classified as idle and shown with a visual marker in the TUI. Default 30.
	IdleThresholdMinutes int `toml:"idle_threshold_minutes"`
	// EvictionTTLMinutes is how long after the last tool call the daemon
	// force-closes an idle connection. 0 disables eviction. Default 60.
	EvictionTTLMinutes int `toml:"eviction_ttl_minutes"`
}

// Config is the resolved configuration for a plumb process.
// Concurrency: read-only after Load returns.
type Config struct {
	LogLevel  string               `toml:"log_level"`
	LogFormat string               `toml:"log_format"`
	LogFile   string               `toml:"log_file"`
	UI        UIConfig             `toml:"ui"`
	Cache     CacheConfig          `toml:"cache"`
	Edits     EditsConfig          `toml:"edits"`
	Walk      WalkConfig           `toml:"walk"`
	Workspace WorkspaceConfig      `toml:"workspace"`
	Git       GitConfig            `toml:"git"`
	Session   SessionConfig        `toml:"session"`
	Quality   QualityConfig        `toml:"quality"`
	Topology  TopologyConfig       `toml:"topology"`
	LSP       map[string]LSPConfig `toml:"lsp"`
	LSPQuery  LSPQueryConfig       `toml:"lsp_query"`
}

var defaults = Config{
	LogLevel:  "info",
	LogFormat: "text",
	UI:        UIConfig{Theme: "nordico", PathStyle: "compact"},
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
	Workspace: WorkspaceConfig{
		AllowDependencyReads: true,
	},
	Git: GitConfig{
		AllowWrites:       true,
		AllowDestructive:  false,
		AllowPush:         false,
		ProtectedBranches: []string{"main", "master"},
	},
	Quality: QualityConfig{
		Enabled:            false,
		Mode:               "background",
		Analysers:          []string{"golangci-lint"},
		TimeoutMs:          2000,
		MaxFindingsPerFile: 5,
	},
	Topology: TopologyConfig{
		Enabled:               true,
		MaxFileSizeBytes:      512 * 1024,
		ResyncBatch:           100,
		ResyncPauseMs:         25,
		ResyncIntervalMinutes: 60,
		Watch:                 true,
	},
	Session: SessionConfig{
		IdleThresholdMinutes: 30,
		EvictionTTLMinutes:   60,
	},
	LSPQuery: LSPQueryConfig{
		Timeout: Duration{30 * time.Second},
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
		"rust": {
			Command:     "rust-analyzer",
			Args:        []string{},
			RootMarkers: []string{"Cargo.toml"},
			Enabled:     false,
		},
		"swift": {
			Command:     "sourcekit-lsp",
			Args:        []string{},
			RootMarkers: []string{"Package.swift"},
			Enabled:     false,
		},
		"zig": {
			Command:     "zls",
			Args:        []string{},
			RootMarkers: []string{"build.zig", "build.zig.zon"},
			Enabled:     false,
		},
		"typescript": {
			Command:     "typescript-language-server",
			Args:        []string{"--stdio"},
			RootMarkers: []string{"tsconfig.json", "jsconfig.json", "package.json"},
			Enabled:     false,
		},
		"kotlin": {
			Command:     "kotlin-language-server",
			Args:        []string{},
			RootMarkers: []string{"settings.gradle.kts", "build.gradle.kts"},
			Enabled:     false,
		},
		"html": {
			Command:     "vscode-html-language-server",
			Args:        []string{"--stdio"},
			RootMarkers: []string{"index.html"},
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
	if cfg.Quality.Analysers != nil {
		out.Quality.Analysers = append([]string(nil), cfg.Quality.Analysers...)
	}
	if cfg.Workspace.ExtraRoots != nil {
		out.Workspace.ExtraRoots = append([]string(nil), cfg.Workspace.ExtraRoots...)
	}
	if cfg.Workspace.ReadRoots != nil {
		out.Workspace.ReadRoots = append([]string(nil), cfg.Workspace.ReadRoots...)
	}
	if cfg.Git.ProtectedBranches != nil {
		out.Git.ProtectedBranches = append([]string(nil), cfg.Git.ProtectedBranches...)
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
	if err := validateQuality(cfg.Quality); err != nil {
		return err
	}
	if cfg.LSPQuery.Timeout.Duration < 0 {
		return fmt.Errorf("lsp_query.timeout must be non-negative (0 disables)")
	}
	if err := validateTopology(cfg.Topology); err != nil {
		return err
	}
	switch cfg.UI.PathStyle {
	case "", "compact", "truncate-middle", "full":
	default:
		return fmt.Errorf("ui.path_style must be compact, truncate-middle, or full; got %q", cfg.UI.PathStyle)
	}
	for name, lsp := range cfg.LSP {
		if lsp.Enabled && lsp.Command == "" {
			return fmt.Errorf("lsp.%s.command must be set when enabled", name)
		}
	}
	return nil
}

func validateQuality(q QualityConfig) error {
	switch q.Mode {
	case "", "background", "sync":
	default:
		return fmt.Errorf("quality.mode must be \"background\" or \"sync\"; got %q", q.Mode)
	}
	if q.TimeoutMs < 0 {
		return fmt.Errorf("quality.timeout_ms must be non-negative")
	}
	if q.MaxFindingsPerFile < 0 {
		return fmt.Errorf("quality.max_findings_per_file must be non-negative")
	}
	return nil
}

func validateTopology(tp TopologyConfig) error {
	if tp.MaxFileSizeBytes < 0 {
		return fmt.Errorf("topology.max_file_size_bytes must be non-negative (0 uses the default)")
	}
	if tp.ResyncBatch < 0 {
		return fmt.Errorf("topology.resync_batch must be non-negative (0 disables pacing)")
	}
	if tp.ResyncPauseMs < 0 {
		return fmt.Errorf("topology.resync_pause_ms must be non-negative (0 disables pacing)")
	}
	if tp.ResyncIntervalMinutes < 0 {
		return fmt.Errorf("topology.resync_interval_minutes must be non-negative (0 disables periodic resync)")
	}
	return nil
}

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

// writeConfigAtomic encodes cfg to a temp file in the target's directory, then
// renames it over path. The rename is atomic on a single filesystem. The temp
// name is dotfile-prefixed so a directory watcher filtering on the "config.toml"
// basename ignores the staging writes and reacts only to the final rename.
// Existing file permissions are preserved; a new file is created 0o644.
func writeConfigAtomic(path string, cfg Config) error {
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
	if err := toml.NewEncoder(tmp).Encode(cfg); err != nil {
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
