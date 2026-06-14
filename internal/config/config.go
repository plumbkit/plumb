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
//
// The package is split across files by concern: this file holds the config type
// definitions; the compiled defaults and deep-clone live in config_defaults.go;
// path resolution, file loading, env overlay, and the per-project merge in
// config_load.go; validation in config_validate.go; atomic persistence in
// config_save.go. The live hot-reloading Store is in store.go.
package config

import "time"

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
	Command     string   `toml:"command"`
	Args        []string `toml:"args"`
	RootMarkers []string `toml:"root_markers"`
	// WeakRootMarkers are promiscuous root markers (e.g. package.json,
	// index.html) that appear in many projects not primarily of that language.
	// Unlike RootMarkers they never identify the language of an ANCESTOR
	// directory during workspace detection — they name the language only of the
	// directory they sit in directly, and only when no RootMarker won. This
	// stops a stray tooling package.json from hijacking a Go/Swift/Rust
	// workspace as TypeScript.
	WeakRootMarkers []string          `toml:"weak_root_markers"`
	Env             map[string]string `toml:"env"`
	// Enabled is the user's intent for this language server. It defaults to true,
	// so an installed server is active automatically; the effective state is
	// Enabled gated on the command being present on PATH (see the cli layer's
	// lspActive), so a language whose server is not installed stays dormant at
	// zero cost. Set false to exclude a language even when its server is
	// installed.
	Enabled bool `toml:"enabled"`
	// IdleTimeout hibernates a language server that has gone this long without a
	// tool call, reclaiming its process memory even while a session stays
	// attached. The poolEntry and its warm cache are kept; the next tool call
	// transparently restarts the process. Aimed at heavyweight servers (jdtls
	// reaches ~1.5 GB RSS). 0 disables hibernation (the default for every
	// language except java). Read at pool construction — restart-needed.
	IdleTimeout Duration `toml:"idle_timeout"`
	// MaxWorkspaces caps the number of simultaneously-running servers of this
	// language across all workspaces. Before starting one beyond the cap, the
	// pool hibernates the least-recently-used running entry of the same language
	// (LRU eviction). 0 means unlimited (the default for every language except
	// java). Read at pool construction — restart-needed.
	MaxWorkspaces int `toml:"max_workspaces"`
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
	// ChildScanDepth bounds how many directory levels below the workspace root
	// the daemon descends to discover language root markers in subdirectories —
	// the monorepo case where the root itself carries only a .plumb/ marker
	// (e.g. core/build.zig + app/Package.swift under one root). Each discovered
	// child language attaches its own server (rooted at the subdirectory) and is
	// listed at session_start; the first is elected the connection's primary.
	// 0 disables the descent. Strong markers only; .git/.plumb/node_modules/build
	// dirs are pruned. Default 2.
	ChildScanDepth int `toml:"child_scan_depth"`
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
	// Default "plumb". Persisted by the TUI theme picker via SaveTheme.
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

// MemoryConfig controls the Advanced Memory Engine: the FTS5 index over the
// markdown memory store, rule-based episodic summaries, and proactive
// memory-hint injection into tool responses. Project-overridable.
//
// Concurrency: read-only after Load returns.
type MemoryConfig struct {
	// Enabled turns on the memory.db FTS5 index. When false, memory tools fall
	// back to the deterministic grep path. Default true.
	Enabled bool `toml:"enabled"`
	// GeneratedSummaries turns on rule-based episodic summaries written when a
	// session goes idle (always redacted; no LLM). Default true.
	GeneratedSummaries bool `toml:"generated_summaries"`
	// InjectHints appends a compact "[Hint: relevant memory …]" block to tool
	// responses when the touched path matches a memory's paths glob. Default true.
	InjectHints bool `toml:"inject_hints"`
	// HintBudgetBytes caps the injected hint block per response. Default 512.
	HintBudgetBytes int `toml:"hint_budget_bytes"`
	// EpisodicBudgetBytes caps the "last session" summary in session_start. Default 1024.
	EpisodicBudgetBytes int `toml:"episodic_budget_bytes"`
	// MaxHints caps how many memories are hinted in one response. Default 3.
	MaxHints int `toml:"max_hints"`
	// IdleSummaryMinutes is the idle threshold before an episodic summary is
	// generated. 0 falls back to Session.IdleThresholdMinutes. Default 0.
	IdleSummaryMinutes int `toml:"idle_summary_minutes"`
	// GeneratedMemoryKeep caps how many generated episodic markdown memories are
	// retained per workspace. 0 disables pruning. Default 50.
	GeneratedMemoryKeep int `toml:"generated_memory_keep"`
}

// SemanticsConfig controls opt-in semantic re-rank for topology_search. Off by
// default. The embedder is always a hosted or user-run HTTP endpoint — plumb
// never bundles or supervises a model. Project-overridable.
//
// Key resolution: APIKey (a literal key in config) wins; when empty, the key is
// read from the environment variable named by APIKeyEnv (or the provider
// preset's default env var). Resolve() applies presets + this rule.
//
// Concurrency: read-only after Load returns.
type SemanticsConfig struct {
	// Enabled turns semantic re-rank on. Default false.
	Enabled bool `toml:"enabled"`
	// Provider selects a preset: openai | voyage | jina | mistral | cohere | custom.
	// Default "openai". "custom" requires BaseURL (a user-run OpenAI-compatible
	// endpoint, e.g. Ollama / llama.cpp / LM Studio / TEI / vLLM).
	Provider string `toml:"provider"`
	// Model is the embedding model id. "" uses the provider preset's default.
	Model string `toml:"model"`
	// BaseURL overrides the provider's API base; required for "custom".
	BaseURL string `toml:"base_url"`
	// APIKey is a literal key. Highest precedence; prefer APIKeyEnv to keep secrets
	// out of config files (a global config is safer than a committed project one).
	APIKey string `toml:"api_key"`
	// APIKeyEnv names the env var holding the key, used when APIKey is empty.
	// "" uses the provider preset's default env var.
	APIKeyEnv string `toml:"api_key_env"`
	// RerankCandidates is how many FTS5 hits to re-rank. Default 50. 0 uses the default.
	RerankCandidates int `toml:"rerank_candidates"`
	// Timeout caps a single embedding HTTP call. Default 10s.
	Timeout Duration `toml:"timeout"`
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
	Semantics SemanticsConfig      `toml:"semantics"`
	Memory    MemoryConfig         `toml:"memory"`
	// Tasks holds per-language build/lint/test/e2e/verify command templates,
	// keyed by the [lsp.<lang>] language id. Executed by the task runner.
	Tasks map[string]TasksConfig `toml:"tasks"`
	// AgentConfigWrites gates whether the agent-writable-config tool may write
	// project config on the user's behalf. Off by default; user-settable only.
	AgentConfigWrites bool `toml:"agent_config_writes"`
}
