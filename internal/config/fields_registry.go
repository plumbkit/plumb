package config

// fields_registry.go holds the registry data literal consumed by fields.go.
// One entry per editable config field; descriptions and reload tiers are the
// canonical copies (the TUI Settings screen reads them from here). Per-language
// families ([lsp.<lang>], [tasks.<lang>]) are single template entries whose
// "<lang>" segment is substituted at display time.
//
// Safety defaults to SafetyDenied (the zero value); the agent allowlist is
// opened explicitly in fields_agent.go.

var minZero = int64(0)

var registryData = []Field{
	// --- Appearance / Logging ---
	{
		Key: "ui.theme", Type: FieldEnum, ReloadTier: ReloadLive,
		Description: "Colour theme for the TUI and `plumb config show` syntax highlighting.",
	},
	{
		Key: "ui.path_style", Type: FieldEnum, ReloadTier: ReloadLive,
		AllowedValues: []string{"compact", "truncate-middle", "full"},
		Description:   "How workspace folder paths are abbreviated in the Sessions sidebar.",
	},
	{
		Key: "log_level", Type: FieldEnum, ReloadTier: ReloadLive,
		AllowedValues: []string{"debug", "info", "warn", "error"},
		Description:   "Daemon log verbosity. Applies live via the control socket.",
	},
	{
		Key: "log_format", Type: FieldEnum, ReloadTier: ReloadRestart,
		AllowedValues: []string{"text", "json"},
		Description:   "Daemon log encoding: human-readable text or structured JSON.",
	},
	{
		Key: "log_file", Type: FieldString, ReloadTier: ReloadRestart,
		Description: "Path the daemon writes logs to. Enter to edit; blank uses the default cache dir.",
	},
	{
		Key: "web.port", Type: FieldInt, ReloadTier: ReloadNextSession, Min: &minZero,
		Description: "Loopback TCP port for the opt-in web UI (`plumb web`). Bound to 127.0.0.1 only; applied on the next `plumb web`. Default 8870.",
	},

	// --- Editing / Walk ---
	{
		Key: "edits.strict", Type: FieldBool, ReloadTier: ReloadLive,
		Description: "Require a prior read_file (matching mtime) before edit_file.",
	},
	{
		Key: "edits.show_write_diff", Type: FieldBool, ReloadTier: ReloadLive,
		Description: "Append a unified diff to edit_file/write_file responses.",
	},
	{
		Key: "edits.rate_limit_per_minute", Type: FieldInt, ReloadTier: ReloadLive, Min: &minZero,
		Description: "Max write ops per session per minute. 0 disables limiting.",
	},
	{
		Key: "edits.post_write_diagnostics_ms", Type: FieldInt, ReloadTier: ReloadLive, Min: &minZero,
		Description: "How long write tools wait for the LSP to re-publish diagnostics. 0 disables.",
	},
	{
		Key: "edits.concurrent_write_skew_ms", Type: FieldInt, ReloadTier: ReloadLive, Min: &minZero,
		Description: "Clock-skew allowance for edit_file's concurrent-write detector. Raise on network mounts.",
	},
	{
		Key: "walk.refuse_home_roots", Type: FieldBool, ReloadTier: ReloadLive,
		Description: "Refuse walks rooted at $HOME or a protected dir (macOS TCC prompt guard).",
	},

	// --- Indexing (topology) ---
	{
		Key: "topology.enabled", Type: FieldBool, ReloadTier: ReloadLive,
		Description: "Enable the SQLite/FTS5 semantic index at <ws>/.plumb/topology.db.",
	},
	{
		Key: "topology.resync_on_attach", Type: FieldBool, ReloadTier: ReloadNextSession,
		Description: "Trigger a full topology resync each time the workspace attaches.",
	},
	{
		Key: "topology.watch", Type: FieldBool, ReloadTier: ReloadNextSession,
		Description: "OS-level file watching: re-index a file the moment it changes on disk.",
	},
	{
		Key: "topology.max_file_size_bytes", Type: FieldInt, ReloadTier: ReloadNextSession, Min: &minZero,
		Description: "Cap on file size considered for extraction (bytes). 0 uses the 512 KiB default.",
	},
	{
		Key: "topology.resync_batch", Type: FieldInt, ReloadTier: ReloadNextSession, Min: &minZero,
		Description: "Files a full resync extracts before pausing. 0 disables pacing.",
	},
	{
		Key: "topology.resync_pause_ms", Type: FieldInt, ReloadTier: ReloadNextSession, Min: &minZero,
		Description: "Pause inserted after each resync batch (ms). 0 disables pacing.",
	},
	{
		Key: "topology.resync_interval_minutes", Type: FieldInt, ReloadTier: ReloadNextSession, Min: &minZero,
		Description: "Periodic full-resync fallback interval. Suppressed while watching; 0 disables.",
	},
	{
		Key: "topology.exclude_patterns", Type: FieldList, ReloadTier: ReloadNextSession,
		Description: "Path globs to skip during indexing. Enter to edit the list.",
	},

	// --- Quality ---
	{
		Key: "quality.enabled", Type: FieldBool, ReloadTier: ReloadNextSession,
		Description: "Run offline post-write analysers (golangci-lint, …) on changed files.",
	},
	{
		Key: "quality.mode", Type: FieldEnum, ReloadTier: ReloadNextSession,
		AllowedValues: []string{"background", "sync"},
		Description:   "background: findings on next request. sync: block and append inline.",
	},
	{
		Key: "quality.timeout_ms", Type: FieldInt, ReloadTier: ReloadNextSession, Min: &minZero,
		Description: "Per-analyser run timeout in milliseconds.",
	},
	{
		Key: "quality.max_findings_per_file", Type: FieldInt, ReloadTier: ReloadNextSession, Min: &minZero,
		Description: "Cap on findings appended per file to keep responses bounded.",
	},
	{
		Key: "quality.analysers", Type: FieldList, ReloadTier: ReloadNextSession,
		Description: "Which analysers to run (e.g. golangci-lint). Enter to edit the list.",
	},

	// --- Git ---
	{
		Key: "git.allow_writes", Type: FieldBool, ReloadTier: ReloadLive,
		Description: "Gate the safe-write tier (add, commit, switch, branch/tag create, stash).",
	},
	{
		Key: "git.allow_destructive", Type: FieldBool, ReloadTier: ReloadLive,
		Description: "Gate reset/clean/checkout/restore/rebase/revert (each call also needs confirm).",
	},
	{
		Key: "git.allow_push", Type: FieldBool, ReloadTier: ReloadLive,
		Description: "Gate push/fetch/pull (each call also needs confirm). Protected branches stay safe.",
	},
	{
		Key: "git.protected_branches", Type: FieldList, ReloadTier: ReloadLive,
		Description: "Branches that may never be force-pushed, even with allow_push. Enter to edit.",
	},

	// --- Session ---
	{
		Key: "session.idle_threshold_minutes", Type: FieldInt, ReloadTier: ReloadLive, Min: &minZero,
		Description: "Minutes with no tool call before a session shows the idle marker (cosmetic).",
	},
	{
		Key: "session.eviction_ttl_minutes", Type: FieldInt, ReloadTier: ReloadLive, Min: &minZero,
		Description: "Minutes idle before the daemon force-closes a connection. 0 disables eviction.",
	},
	{
		Key: "session.persist_state", Type: FieldBool, ReloadTier: ReloadNextSession,
		Description: "Persist read-tracking + pinned workspace so they survive a daemon restart (rehydrated on proxy reconnect).",
	},
	{
		Key: "session.persist_state_ttl_minutes", Type: FieldInt, ReloadTier: ReloadLive, Min: &minZero,
		Description: "Minutes persisted per-connection state lingers before the daemon prunes it. Default 1440 (24h).",
	},

	// --- Memory ---
	{
		Key: "memory.enabled", Type: FieldBool, ReloadTier: ReloadNextSession,
		Description: "The memory.db FTS5 index. Off ⇒ search_memories uses grep only.",
	},
	{
		Key: "memory.generated_summaries", Type: FieldBool, ReloadTier: ReloadNextSession,
		Description: "Rule-based episodic summaries (no LLM) written when a session goes idle.",
	},
	{
		Key: "memory.inject_hints", Type: FieldBool, ReloadTier: ReloadNextSession,
		Description: "Append a \"[Hint: relevant memory …]\" block to path-bearing tool responses.",
	},
	{
		Key: "memory.hint_budget_bytes", Type: FieldInt, ReloadTier: ReloadNextSession, Min: &minZero,
		Description: "Byte cap on an injected hint block.",
	},
	{
		Key: "memory.episodic_budget_bytes", Type: FieldInt, ReloadTier: ReloadNextSession, Min: &minZero,
		Description: "Byte cap on the session_start \"last session\" summary.",
	},
	{
		Key: "memory.max_hints", Type: FieldInt, ReloadTier: ReloadNextSession, Min: &minZero,
		Description: "Max memories hinted per response.",
	},
	{
		Key: "memory.idle_summary_minutes", Type: FieldInt, ReloadTier: ReloadNextSession, Min: &minZero,
		Description: "Idle threshold for episodic generation. 0 falls back to session.idle_threshold_minutes.",
	},
	{
		Key: "memory.generated_memory_keep", Type: FieldInt, ReloadTier: ReloadNextSession, Min: &minZero,
		Description: "Newest episodic-* markdown memories retained per workspace. 0 disables pruning.",
	},

	// --- Workspace ---
	{
		Key: "workspace.auto_attach", Type: FieldBool, ReloadTier: ReloadNextSession,
		Description: "Fall back to the nearest .git/ or seed dir when no marker is found (LSP unavailable).",
	},
	{
		Key: "workspace.auto_attach_persist", Type: FieldBool, ReloadTier: ReloadNextSession,
		Description: "Create .plumb/ at the synthetic root on first auto-attach (implies auto_attach).",
	},
	{
		Key: "workspace.allow_dependency_reads", Type: FieldBool, ReloadTier: ReloadNextSession,
		Description: "Let read/search tools reach the Go module cache + GOROOT read-only.",
	},
	{
		Key: "workspace.child_scan_depth", Type: FieldInt, ReloadTier: ReloadNextSession, Min: &minZero,
		Description: "Levels below the root to scan for language markers in subdirs (monorepo). 0 disables.",
	},
	{
		Key: "workspace.extra_roots", Type: FieldList, ReloadTier: ReloadNextSession,
		Description: "Extra dirs read+write tools may reach beyond the workspace. Enter to edit.",
	},
	{
		Key: "workspace.read_roots", Type: FieldList, ReloadTier: ReloadNextSession,
		Description: "Extra read-only dirs (compare another project). Enter to edit the list.",
	},

	// --- Others (cache / lsp_query) ---
	{
		Key: "cache.ttl", Type: FieldDuration, ReloadTier: ReloadRestart,
		Description: "Session symbol-cache time-to-live. Needs a daemon restart.",
	},
	{
		Key: "cache.max_size", Type: FieldInt, ReloadTier: ReloadRestart, Min: &minZero,
		Description: "Max entries in the session symbol cache. Needs a daemon restart.",
	},
	{
		Key: "lsp_query.timeout", Type: FieldDuration, ReloadTier: ReloadNextSession,
		Description: "Cap on a single LSP tool call when the caller carries no deadline. 0 disables.",
	},
	{
		Key: "agent_config_writes", Type: FieldBool, ReloadTier: ReloadNextSession,
		Description: "Allow the agent-writable-config tool to write project config (a small allowlist). Off by default; user-settable only.",
	},

	// --- LSP per-language template ([lsp.<lang>]) ---
	{
		Key: "lsp.<lang>.enabled", Type: FieldBool, ReloadTier: ReloadNextSession, PerLanguage: true,
		Description: "Enabled languages activate automatically once their server is installed. Set off to exclude <lang> even when installed.",
	},
	{
		Key: "lsp.<lang>.command", Type: FieldString, ReloadTier: ReloadNextSession, PerLanguage: true,
		Description: "Executable for the <lang> language server. Enter to edit.",
	},
	{
		Key: "lsp.<lang>.args", Type: FieldList, ReloadTier: ReloadNextSession, PerLanguage: true,
		Description: "Command-line args passed to the <lang> server. Enter to edit.",
	},
	{
		Key: "lsp.<lang>.root_markers", Type: FieldList, ReloadTier: ReloadNextSession, PerLanguage: true,
		Description: "Files that mark a <lang> project root. Enter to edit.",
	},

	// --- Semantics ---
	{
		Key: "semantics.enabled", Type: FieldBool, ReloadTier: ReloadLive,
		Description: "Re-rank topology_search results by meaning, via your chosen embedding API. Off by default.",
	},
	{
		Key: "semantics.provider", Type: FieldEnum, ReloadTier: ReloadLive,
		AllowedValues: SemanticsProviders,
		Description:   "openai | voyage (code) | jina | mistral | cohere | custom (your own OpenAI-compatible endpoint).",
	},
	{
		Key: "semantics.model", Type: FieldString, ReloadTier: ReloadLive,
		Description: "Embedding model id. Blank uses the provider's default (e.g. voyage-code-3).",
	},
	{
		Key: "semantics.base_url", Type: FieldString, ReloadTier: ReloadLive,
		Description: "API base URL. Blank uses the provider preset; required for custom (e.g. http://localhost:11434/v1).",
	},
	{
		Key: "semantics.api_key_env", Type: FieldString, ReloadTier: ReloadLive,
		Description: "Name of the env var holding the API key (used when 'api key' is blank). ✓ = the var is set.",
	},
	{
		Key: "semantics.api_key", Type: FieldString, ReloadTier: ReloadLive, Secret: true,
		Description: "Key stored in config; takes precedence over the env var. Prefer the env var to keep secrets out of files.",
	},
	{
		Key: "semantics.rerank_candidates", Type: FieldInt, ReloadTier: ReloadLive, Min: &minZero,
		Description: "How many FTS5 hits to re-rank semantically. Default 50.",
	},
	{
		Key: "semantics.timeout", Type: FieldDuration, ReloadTier: ReloadLive,
		Description: "Cap on a single embedding API call. Default 10s.",
	},

	// --- Task commands per-language template ([tasks.<lang>]) ---
	{
		Key: "tasks.<lang>.build", Type: FieldString, ReloadTier: ReloadLive, PerLanguage: true,
		Description: "Build command for <lang> projects (a single argv; no shell). Run by `plumb build` and the run_task tool.",
	},
	{
		Key: "tasks.<lang>.lint", Type: FieldString, ReloadTier: ReloadLive, PerLanguage: true,
		Description: "Lint command for <lang> projects (a single argv; no shell).",
	},
	{
		Key: "tasks.<lang>.test", Type: FieldString, ReloadTier: ReloadLive, PerLanguage: true,
		Description: "Test command for <lang> projects (a single argv; no shell). May contain a {target} placeholder.",
	},
	{
		Key: "tasks.<lang>.e2e", Type: FieldString, ReloadTier: ReloadLive, PerLanguage: true,
		Description: "End-to-end/integration test command for <lang> projects (a single argv; no shell).",
	},
	{
		Key: "tasks.<lang>.verify", Type: FieldString, ReloadTier: ReloadLive, PerLanguage: true,
		Description: "Composite gate for <lang>: runs the build slot then the test slot in sequence.",
	},
}
