package tui

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

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
// Memory section's collector), with git linked worktrees collapsed onto their
// repository's main worktree — the throwaway worktrees a multi-agent session
// spawns (.claude/worktrees/*) are checkout dirs of one repo, not separate
// workspaces, and must not each get a scope row. Stable order so the cursor
// never jumps.
func (m *Model) collectSettingsScopes() []settingScope {
	wss := m.collectMemoryWorkspaces()
	seen := make(map[string]bool, len(wss))
	folders := make([]string, 0, len(wss))
	for _, ws := range wss {
		folder := mainWorktreeOf(ws.Folder)
		if seen[folder] {
			continue
		}
		seen[folder] = true
		folders = append(folders, folder)
	}
	sort.Strings(folders)
	scopes := make([]settingScope, 0, 1+len(folders))
	scopes = append(scopes, settingScope{global: true, label: "Global"})
	for _, f := range folders {
		scopes = append(scopes, settingScope{folder: f, label: filepath.Base(f)})
	}
	return scopes
}

// mainWorktreeOf maps a workspace folder to the main worktree of its git
// repository when the folder is a linked worktree (a `.claude/worktrees/*`
// checkout or a `git worktree add`), and to the folder itself in every other
// case. Pure filesystem layout — no git subprocess: a linked worktree's `.git`
// is a file `gitdir: <common>/worktrees/<name>`, and the main worktree is the
// nearest ancestor whose `.git` is (or points at) <common>. Any doubt — no
// `.git` pointer, a target that is not a worktree admin dir, no matching
// ancestor (a worktree checked out outside the repo tree) — leaves the folder
// alone, so an unrecognised layout degrades to one-entry-per-folder.
func mainWorktreeOf(folder string) string {
	gitdir, ok := gitdirPointer(folder)
	if !ok || filepath.Base(filepath.Dir(gitdir)) != "worktrees" {
		return folder // plain repo, a submodule's main worktree, or no repo at all
	}
	common := filepath.Dir(filepath.Dir(gitdir))
	for dir := folder; ; dir = filepath.Dir(dir) {
		if target, ok := gitdirPointer(dir); ok && target == common {
			return dir // submodule-style main worktree: .git points at the common dir
		}
		if filepath.Join(dir, ".git") == common {
			return dir // plain repository: its .git directory is the common dir
		}
		if parent := filepath.Dir(dir); parent == dir {
			return folder // reached the filesystem root without a match
		}
	}
}

// gitdirPointer reads dir/.git when it is git's pointer FILE (a linked
// worktree or a submodule checkout) and returns the resolved gitdir target.
// The bool is false when .git is a directory (a repository's main worktree)
// or missing, and when the file does not carry a parseable gitdir pointer.
func gitdirPointer(dir string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(dir, ".git"))
	if err != nil {
		return "", false
	}
	s := strings.TrimSpace(string(data))
	const prefix = "gitdir:"
	if !strings.HasPrefix(s, prefix) {
		return "", false
	}
	target := strings.TrimSpace(strings.TrimPrefix(s, prefix))
	if target == "" {
		return "", false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	return filepath.Clean(target), true
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
// project-overridable fields, with effective values merged from global.
//
// Every row's value comes from the MERGED config, which is the config as plumb
// actually resolves it — so a capability-granting key the project sets but plumb
// is ignoring (an untrusted root) shows the global value that is really in
// force, and is flagged notInEffect rather than overridden. A row must never
// display the project file's value as though it were live.
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
	policy, policyErr := projectPolicyStatus(scope.folder)
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
		if rawHasPath(raw, path) {
			it.overridden, it.notInEffect = scopeRowState(policy, policyErr, path)
		}
		out = append(out, it)
	}
	return out
}

// projectPolicyStatus is the seam through which the settings rows read trust
// state. A package-level indirection so a TUI test can substitute one rather than
// reading — and depending on the contents of — the developer's real
// <DataDir>/trust.json.
var projectPolicyStatus = config.ProjectPolicyStatusFor

// scopeRowState decides how a row the project config sets should present: a live
// override, or set-and-ignored.
//
// A key the project sets but which is not trusted is set-and-ignored, never
// "override" — Asked() is the authority, being true exactly for the keys
// LoadProject gates on trust. When the status could not be read at all, a
// capability-granting row presents as ignored rather than live: LoadProject fails
// closed on the same fault and will have forced the value back, so claiming the
// row is live would have the display disagree with the config in the one
// direction that misleads.
func scopeRowState(policy config.ProjectPolicyStatus, policyErr error, path []string) (overridden, notInEffect bool) {
	key := strings.Join(path, ".")
	if policyErr != nil {
		return !isCapabilityKey(key), isCapabilityKey(key)
	}
	if policy.NeedsTrust() && policy.Asked(key) {
		return false, true
	}
	return true, false
}

// isCapabilityKey reports whether a dotted settings key belongs to a section
// LoadProject gates on trust, used only to decide the safe presentation when the
// trust status itself is unreadable.
//
// It delegates rather than enumerating, because the two lists getting out of step
// is a silent display bug: a gated key this did not recognise would present as a
// live override while LoadProject had in fact forced it back. Note the answer is
// per-KEY within [collab], not per-section — the channel switches are gated, the
// budgets beside them are not.
func isCapabilityKey(key string) bool { return config.IsGatedProjectKey(key) }

// itemTOMLPath returns the TOML key path for a row, handling the dynamic
// per-language [lsp.<lang>] rows (whose path depends on lspLang) and delegating
// to the static tomlPath for everything else. The bool is false for global-only
// settings (hidden in a workspace scope).
//
// Every named [lsp.<lang>] field is workspace-writable, including the
// exec-deciding ones. They were withdrawn when a project config could never have
// them honoured — a control that writes TOML plumb ignores is worse than no
// control — and restored once `plumb trust` made a workspace-scope edit
// meaningful. What was worse than no control is now handled honestly instead: an
// untrusted row is marked notInEffect, shows the value actually in force, and
// says so when edited.
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
//
// A workspace-scope write that lands on a capability-granting key of an
// untrusted root SAYS SO. Writing the file and reporting a plain success would
// be the original complaint restored: the user would believe they had configured
// something. The refresh inside applyScopedAt has already re-evaluated the row,
// so the flag consulted here is the state after the write.
func (m Model) scopedStatus(key settingKey, change string) string {
	if m.currentScope().global {
		return settingStatus(key, change)
	}
	if m.focusedRowNotInEffect() {
		return change + " · written, NOT in effect — run `plumb trust` in this workspace"
	}
	return change + " · workspace override"
}

// focusedRowNotInEffect reports whether the highlighted row is a project
// override plumb is currently ignoring. Every scoped edit is initiated from the
// focused row, so this identifies the row just written without threading its
// identity through each of the dozen apply paths.
func (m Model) focusedRowNotInEffect() bool {
	if m.settingsCursor < 0 || m.settingsCursor >= len(m.settingsItems) {
		return false
	}
	return m.settingsItems[m.settingsCursor].notInEffect
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
	skFsync:                      {"edits", "fsync"},
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
	skTopoExtractTimeoutSec:      {"topology", "extract_timeout_seconds"},
	skTopoResyncBatch:            {"topology", "resync_batch"},
	skTopoResyncPauseMs:          {"topology", "resync_pause_ms"},
	skTopoResyncIntervalMin:      {"topology", "resync_interval_minutes"},
	skQuality:                    {"quality", "enabled"},
	skQualityMode:                {"quality", "mode"},
	skQualityTimeoutMs:           {"quality", "timeout_ms"},
	skQualityMaxFindings:         {"quality", "max_findings_per_file"},
	// The [git] tier rows are project-overridable, but only take effect on a
	// workspace the user has trusted with `plumb trust` — LoadProject forces the
	// whole block back to the global config otherwise, because a cloned repo's
	// .plumb/config.toml would else grant itself history destruction and pushes to
	// the user's remotes. Until then the row renders notInEffect (see
	// buildScopeItems), showing the global value that is actually in force.
	skGitWrites:            {"git", "allow_writes"},
	skGitDestructive:       {"git", "allow_destructive"},
	skGitPush:              {"git", "allow_push"},
	skGitCommitTrailer:     {"git", "commit_trailer"},
	skProtectedBranches:    {"git", "protected_branches"},
	skAutoAttach:           {"workspace", "auto_attach"},
	skAutoAttachPersist:    {"workspace", "auto_attach_persist"},
	skAllowDependencyReads: {"workspace", "allow_dependency_reads"},
	skChildScanDepth:       {"workspace", "child_scan_depth"},
	// extra_roots/read_roots are global-only: LoadProject forces them back to base
	// from an (untrusted) project config, so a workspace-scope override would never
	// take effect. They are shown only in the Global scope (and remain in
	// settingDottedKeys for that).
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
	skCollabCrossProject:        {"collab", "cross_project"},
	skCollabMaxExchanges:        {"collab", "max_exchanges"},
	skCollabChatBudgetBytes:     {"collab", "chat_budget_bytes"},
	skCollabMaxWaitSec:          {"collab", "max_wait_seconds"},
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
	skFsync:                      "edits.fsync",
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
	skTopoExtractTimeoutSec:      "topology.extract_timeout_seconds",
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
	skGitCommitTrailer:           "git.commit_trailer",
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
	skCollabCrossProject:         "collab.cross_project",
	skCollabMaxExchanges:         "collab.max_exchanges",
	skCollabChatBudgetBytes:      "collab.chat_budget_bytes",
	skCollabMaxWaitSec:           "collab.max_wait_seconds",
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
