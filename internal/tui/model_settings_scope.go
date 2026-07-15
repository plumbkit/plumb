package tui

import (
	"github.com/plumbkit/plumb/internal/config"
)

// settingScope identifies which configuration the Settings screen is editing:
// the global config (Global, index 0) or one workspace's .plumb/config.toml.
type settingScope struct {
	global bool
	folder string // workspace root when !global
	label  string // "Global" or filepath.Base(folder)
}

// collectSettingsScopes builds the scope column: Global first, then one entry
// per active workspace (deduped sessions + the TUI launch dir, reusing the
// Memory section's collector). Stable order so the cursor never jumps.
func (m *Model) collectSettingsScopes() []settingScope {
	wss := m.collectMemoryWorkspaces()
	scopes := make([]settingScope, 0, 1+len(wss))
	scopes = append(scopes, settingScope{global: true, label: "Global"})
	for _, ws := range wss {
		scopes = append(scopes, settingScope{folder: ws.Folder, label: ws.Label})
	}
	return scopes
}

// currentScope returns the selected scope, defaulting to Global.
func (m Model) currentScope() settingScope {
	if m.settingsScopeCursor > 0 && m.settingsScopeCursor < len(m.settingsScopes) {
		return m.settingsScopes[m.settingsScopeCursor]
	}
	return settingScope{global: true, label: "Global"}
}

// buildScopeItems builds the settings rows for the selected scope. Global shows
// every field from the global snapshot; a workspace shows only the
// project-overridable fields, with effective values merged from global and an
// `overridden` flag set when the key is present in that project's config file.
func (m *Model) buildScopeItems() []settingItem {
	scope := m.currentScope()
	if scope.global {
		return buildSettingItems(m.settingsCfg)
	}
	merged, err := config.LoadProject(m.settingsCfg, scope.folder)
	if err != nil {
		merged = m.settingsCfg
	}
	raw, _ := config.LoadProjectRaw(scope.folder)
	out := make([]settingItem, 0, len(buildSettingItems(merged)))
	for _, it := range buildSettingItems(merged) {
		if storeBackedWorkspaceKey(it.key) {
			// A manual, out-of-repo per-workspace grant (WorkspaceRootsStore), not a
			// project-config override — populate the row from the store.
			out = append(out, applyStoreRoots(it, scope.folder))
			continue
		}
		path, ok := itemTOMLPath(it)
		if !ok { // global-only setting: hidden in a workspace scope
			continue
		}
		it.overridden = rawHasPath(raw, path)
		out = append(out, it)
	}
	return out
}

// itemTOMLPath returns the TOML key path for a row, handling the dynamic
// per-language [lsp.<lang>] rows (whose path depends on lspLang) and delegating
// to the static tomlPath for everything else. The bool is false for global-only
// settings (hidden in a workspace scope).
func itemTOMLPath(it settingItem) ([]string, bool) {
	if it.lspLang != "" {
		field, ok := lspFieldName(it.key)
		if !ok {
			return nil, false
		}
		return []string{"lsp", it.lspLang, field}, true
	}
	return tomlPath(it.key)
}

// lspFieldName maps an LSP setting key to its TOML field name under [lsp.<lang>].
func lspFieldName(key settingKey) (string, bool) {
	switch key {
	case skLSPEnabled:
		return "enabled", true
	case skLSPCommand:
		return "command", true
	case skLSPArgs:
		return "args", true
	case skLSPRootMarkers:
		return "root_markers", true
	case skLSPDiagnostics:
		return "diagnostics", true
	default:
		return "", false
	}
}

// applyLSPField mutates the [lsp.<lang>] entry for the given field on c. Used as
// the apply closure for both the global save and the workspace sparse write.
func applyLSPField(c *config.Config, lang string, key settingKey, value any) {
	if c.LSP == nil {
		c.LSP = map[string]config.LSPConfig{}
	}
	e := c.LSP[lang]
	switch key {
	case skLSPEnabled:
		e.Enabled, _ = value.(bool)
	case skLSPCommand:
		e.Command, _ = value.(string)
	case skLSPArgs:
		e.Args, _ = value.([]string)
	case skLSPRootMarkers:
		e.RootMarkers, _ = value.([]string)
	case skLSPDiagnostics:
		e.Diagnostics, _ = value.(string)
	}
	c.LSP[lang] = e
}

// applyScopedLSP persists an LSP field change (value) for the row's language in
// the current scope and refreshes the rows.
func (m *Model) applyScopedLSP(it settingItem, value any) bool {
	path, ok := itemTOMLPath(it)
	if !ok {
		return false
	}
	lang, key := it.lspLang, it.key
	return m.applyScopedAt(path, value, func(c *config.Config) { applyLSPField(c, lang, key, value) })
}

// applyScopedSetting persists value for key in the current scope and refreshes
// the rows. Global scope writes the whole config (apply mutates the snapshot
// and pushes reload-config); a workspace writes only the key sparsely to its
// .plumb/config.toml and pushes reload-project. Returns true on success.
func (m *Model) applyScopedSetting(key settingKey, value any, apply func(*config.Config)) bool {
	path, _ := tomlPath(key)
	return m.applyScopedAt(path, value, apply)
}

// applyScopedAt persists value at the explicit TOML path in the current scope.
// Global scope runs the full-config save (apply mutates the loaded config and
// the snapshot, then pushes reload-config); a workspace writes only path
// sparsely to its .plumb/config.toml and pushes reload-project. path may be nil
// in Global scope (the apply closure is authoritative there); a workspace write
// with no path is refused. Returns true on success.
func (m *Model) applyScopedAt(path []string, value any, apply func(*config.Config)) bool {
	scope := m.currentScope()
	if scope.global {
		if !m.persist(apply) {
			return false
		}
		apply(&m.settingsCfg)
		m.refreshSettingsItems()
		return true
	}
	if len(path) == 0 {
		return false
	}
	if err := config.SetProjectValue(scope.folder, path, value); err != nil {
		m.settingsStatus = "save failed: " + err.Error()
		return false
	}
	m.pendingProjectReload = scope.folder
	m.refreshSettingsItems() // re-reads the project file → the override shows
	return true
}

// resetToInherit removes the focused row's key from the workspace config (the
// "inherit" state — it falls back to global/default). No-op in Global scope.
func (m Model) resetToInherit() Model {
	scope := m.currentScope()
	if scope.global || m.settingsCursor < 0 || m.settingsCursor >= len(m.settingsItems) {
		return m
	}
	it := m.settingsItems[m.settingsCursor]
	if storeBackedWorkspaceKey(it.key) {
		if m.writeWorkspaceRoots(it.key, nil) {
			m.settingsStatus = it.label + " → inherit"
		}
		return m
	}
	path, ok := itemTOMLPath(it)
	if !ok {
		return m
	}
	if err := config.UnsetProjectValue(scope.folder, path); err != nil {
		m.settingsStatus = "reset failed: " + err.Error()
		return m
	}
	m.pendingProjectReload = scope.folder
	m.refreshSettingsItems()
	m.settingsStatus = it.label + " → inherit"
	return m
}

// scopedStatus formats the post-change status for the current scope.
func (m Model) scopedStatus(key settingKey, change string) string {
	if m.currentScope().global {
		return settingStatus(key, change)
	}
	return change + " · workspace override"
}

// rawHasPath reports whether the dotted key path is present in a raw project
// config map (nested map[string]any from config.LoadProjectRaw).
func rawHasPath(m map[string]any, path []string) bool {
	for _, k := range path[:len(path)-1] {
		next, ok := m[k].(map[string]any)
		if !ok {
			return false
		}
		m = next
	}
	_, ok := m[path[len(path)-1]]
	return ok
}

// settingTOMLPaths is the single source of truth for which settings are
// project-overridable and where they live in TOML. A key absent here is
// global-only ([ui], logging, cache, lsp_query, session — applied daemon-wide
// even though LoadProject merges them), so it never appears in a workspace scope.
var settingTOMLPaths = map[settingKey][]string{
	skStrict:                     {"edits", "strict"},
	skShowWriteDiff:              {"edits", "show_write_diff"},
	skBlockDirtyWrites:           {"edits", "block_dirty_writes"},
	skRateLimit:                  {"edits", "rate_limit_per_minute"},
	skPostWriteDiagMs:            {"edits", "post_write_diagnostics_ms"},
	skPostWriteCrossFile:         {"edits", "post_write_cross_file"},
	skPostWriteCrossFileSettleMs: {"edits", "post_write_cross_file_settle_ms"},
	skConcurrentSkewMs:           {"edits", "concurrent_write_skew_ms"},
	skRefuseHomeRoots:            {"walk", "refuse_home_roots"},
	skTopology:                   {"topology", "enabled"},
	skTopoResyncOnAttach:         {"topology", "resync_on_attach"},
	skTopoWatch:                  {"topology", "watch"},
	skTopoMaxFileSize:            {"topology", "max_file_size_bytes"},
	skTopoResyncBatch:            {"topology", "resync_batch"},
	skTopoResyncPauseMs:          {"topology", "resync_pause_ms"},
	skTopoResyncIntervalMin:      {"topology", "resync_interval_minutes"},
	skQuality:                    {"quality", "enabled"},
	skQualityMode:                {"quality", "mode"},
	skQualityTimeoutMs:           {"quality", "timeout_ms"},
	skQualityMaxFindings:         {"quality", "max_findings_per_file"},
	skGitWrites:                  {"git", "allow_writes"},
	skGitDestructive:             {"git", "allow_destructive"},
	skGitPush:                    {"git", "allow_push"},
	skAutoAttach:                 {"workspace", "auto_attach"},
	skAutoAttachPersist:          {"workspace", "auto_attach_persist"},
	skAllowDependencyReads:       {"workspace", "allow_dependency_reads"},
	skChildScanDepth:             {"workspace", "child_scan_depth"},
	// extra_roots/read_roots are global-only: LoadProject forces them back to base
	// from an (untrusted) project config, so a workspace-scope override would never
	// take effect. They are shown only in the Global scope (and remain in
	// settingDottedKeys for that).
	skProtectedBranches:         {"git", "protected_branches"},
	skExcludePatterns:           {"topology", "exclude_patterns"},
	skAnalysers:                 {"quality", "analysers"},
	skMemoryEnabled:             {"memory", "enabled"},
	skMemoryGeneratedSummaries:  {"memory", "generated_summaries"},
	skMemoryInjectHints:         {"memory", "inject_hints"},
	skMemoryHintBudgetBytes:     {"memory", "hint_budget_bytes"},
	skMemoryEpisodicBudgetBytes: {"memory", "episodic_budget_bytes"},
	skMemoryMaxHints:            {"memory", "max_hints"},
	skMemoryIdleSummaryMin:      {"memory", "idle_summary_minutes"},
	skMemoryGeneratedKeep:       {"memory", "generated_memory_keep"},
	skCollabPeerAwareness:       {"collab", "peer_awareness"},
	skCollabHintBudgetBytes:     {"collab", "hint_budget_bytes"},
	skCollabIntents:             {"collab", "intents"},
	skCollabMailbox:             {"collab", "mailbox"},
	skCollabKnowledgeHandoff:    {"collab", "knowledge_handoff"},
	skCollabIntentTTLMin:        {"collab", "intent_ttl_minutes"},
	skRastroEnabled:             {"rastro", "enabled"},
	skRastroPath:                {"rastro", "path"},
	skXcodeAutoBuildServer:      {"xcode", "auto_build_server"},
	skXcodeScheme:               {"xcode", "scheme"},
	skXcodeTimeout:              {"xcode", "timeout"},
	// agent_config_writes is deliberately ABSENT: it is a global-only safety knob
	// (LoadProject forces the global value to win), so it never appears in a
	// workspace scope — a project config cannot enable agent writes.
}

// tomlPath returns the TOML key path for a project-overridable setting and
// whether it is project-overridable at all.
func tomlPath(key settingKey) ([]string, bool) {
	p, ok := settingTOMLPaths[key]
	return p, ok
}

// settingDottedKeys maps every settings row key to its config-field-registry
// dotted key. It is a superset of settingTOMLPaths: the global-only rows (theme,
// logging, cache, lsp_query, session, semantics) that LoadProject does not
// override per-project still need a registry identity for help text and reload
// tier. The per-language [lsp.<lang>] rows are resolved by dottedKeyFor, not
// here. TestSettingsRegistryDrift keeps this in step with settingTOMLPaths.
var settingDottedKeys = map[settingKey]string{
	skTheme:                      "ui.theme",
	skPathStyle:                  "ui.path_style",
	skWebPort:                    "web.port",
	skLogLevel:                   "log_level",
	skLogFormat:                  "log_format",
	skLogFile:                    "log_file",
	skStrict:                     "edits.strict",
	skShowWriteDiff:              "edits.show_write_diff",
	skBlockDirtyWrites:           "edits.block_dirty_writes",
	skRateLimit:                  "edits.rate_limit_per_minute",
	skPostWriteDiagMs:            "edits.post_write_diagnostics_ms",
	skPostWriteCrossFile:         "edits.post_write_cross_file",
	skPostWriteCrossFileSettleMs: "edits.post_write_cross_file_settle_ms",
	skConcurrentSkewMs:           "edits.concurrent_write_skew_ms",
	skRefuseHomeRoots:            "walk.refuse_home_roots",
	skTopology:                   "topology.enabled",
	skTopoResyncOnAttach:         "topology.resync_on_attach",
	skTopoWatch:                  "topology.watch",
	skTopoMaxFileSize:            "topology.max_file_size_bytes",
	skTopoResyncBatch:            "topology.resync_batch",
	skTopoResyncPauseMs:          "topology.resync_pause_ms",
	skTopoResyncIntervalMin:      "topology.resync_interval_minutes",
	skExcludePatterns:            "topology.exclude_patterns",
	skQuality:                    "quality.enabled",
	skQualityMode:                "quality.mode",
	skQualityTimeoutMs:           "quality.timeout_ms",
	skQualityMaxFindings:         "quality.max_findings_per_file",
	skAnalysers:                  "quality.analysers",
	skGitWrites:                  "git.allow_writes",
	skGitDestructive:             "git.allow_destructive",
	skGitPush:                    "git.allow_push",
	skProtectedBranches:          "git.protected_branches",
	skIdleThresholdMin:           "session.idle_threshold_minutes",
	skEvictionTTLMin:             "session.eviction_ttl_minutes",
	skPersistState:               "session.persist_state",
	skPersistStateTTLMin:         "session.persist_state_ttl_minutes",
	skMemoryEnabled:              "memory.enabled",
	skMemoryGeneratedSummaries:   "memory.generated_summaries",
	skMemoryInjectHints:          "memory.inject_hints",
	skMemoryHintBudgetBytes:      "memory.hint_budget_bytes",
	skMemoryEpisodicBudgetBytes:  "memory.episodic_budget_bytes",
	skMemoryMaxHints:             "memory.max_hints",
	skMemoryIdleSummaryMin:       "memory.idle_summary_minutes",
	skMemoryGeneratedKeep:        "memory.generated_memory_keep",
	skCollabPeerAwareness:        "collab.peer_awareness",
	skCollabHintBudgetBytes:      "collab.hint_budget_bytes",
	skCollabIntents:              "collab.intents",
	skCollabMailbox:              "collab.mailbox",
	skCollabKnowledgeHandoff:     "collab.knowledge_handoff",
	skCollabIntentTTLMin:         "collab.intent_ttl_minutes",
	skRastroEnabled:              "rastro.enabled",
	skRastroPath:                 "rastro.path",
	skXcodeAutoBuildServer:       "xcode.auto_build_server",
	skXcodeScheme:                "xcode.scheme",
	skXcodeTimeout:               "xcode.timeout",
	skAutoAttach:                 "workspace.auto_attach",
	skAutoAttachPersist:          "workspace.auto_attach_persist",
	skAllowDependencyReads:       "workspace.allow_dependency_reads",
	skChildScanDepth:             "workspace.child_scan_depth",
	skExtraRoots:                 "workspace.extra_roots",
	skReadRoots:                  "workspace.read_roots",
	skCacheTTL:                   "cache.ttl",
	skCacheMaxSize:               "cache.max_size",
	skLSPTimeout:                 "lsp_query.timeout",
	skSemEnabled:                 "semantics.enabled",
	skSemProvider:                "semantics.provider",
	skSemModel:                   "semantics.model",
	skSemBaseURL:                 "semantics.base_url",
	skSemAPIKeyEnv:               "semantics.api_key_env",
	skSemAPIKey:                  "semantics.api_key",
	skSemRerankCandidates:        "semantics.rerank_candidates",
	skSemTimeout:                 "semantics.timeout",
	skAgentConfigWrites:          "agent_config_writes",
}
