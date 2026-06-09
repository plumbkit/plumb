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
	skStrict:                {"edits", "strict"},
	skShowWriteDiff:         {"edits", "show_write_diff"},
	skRateLimit:             {"edits", "rate_limit_per_minute"},
	skPostWriteDiagMs:       {"edits", "post_write_diagnostics_ms"},
	skConcurrentSkewMs:      {"edits", "concurrent_write_skew_ms"},
	skRefuseHomeRoots:       {"walk", "refuse_home_roots"},
	skTopology:              {"topology", "enabled"},
	skTopoResyncOnAttach:    {"topology", "resync_on_attach"},
	skTopoWatch:             {"topology", "watch"},
	skTopoMaxFileSize:       {"topology", "max_file_size_bytes"},
	skTopoResyncBatch:       {"topology", "resync_batch"},
	skTopoResyncPauseMs:     {"topology", "resync_pause_ms"},
	skTopoResyncIntervalMin: {"topology", "resync_interval_minutes"},
	skQuality:               {"quality", "enabled"},
	skQualityMode:           {"quality", "mode"},
	skQualityTimeoutMs:      {"quality", "timeout_ms"},
	skQualityMaxFindings:    {"quality", "max_findings_per_file"},
	skGitWrites:             {"git", "allow_writes"},
	skGitDestructive:        {"git", "allow_destructive"},
	skGitPush:               {"git", "allow_push"},
	skAutoAttach:            {"workspace", "auto_attach"},
	skAutoAttachPersist:     {"workspace", "auto_attach_persist"},
	skAllowDependencyReads:  {"workspace", "allow_dependency_reads"},
	skExtraRoots:            {"workspace", "extra_roots"},
	skReadRoots:             {"workspace", "read_roots"},
	skProtectedBranches:     {"git", "protected_branches"},
	skExcludePatterns:       {"topology", "exclude_patterns"},
	skAnalysers:             {"quality", "analysers"},
}

// tomlPath returns the TOML key path for a project-overridable setting and
// whether it is project-overridable at all.
func tomlPath(key settingKey) ([]string, bool) {
	p, ok := settingTOMLPaths[key]
	return p, ok
}
