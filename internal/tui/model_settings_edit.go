package tui

// model_settings_edit.go — row activation (enter/space) and the list / text
// value editors: opening, key routing, commit, and effective-value reads.

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/plumbkit/plumb/internal/config"
)

// activateSetting handles enter/space: opens the theme picker for the popup row
// and flips toggles; numeric and cycle rows are changed with ←→ instead.
func (m Model) activateSetting() Model {
	if m.settingsCursor < 0 || m.settingsCursor >= len(m.settingsItems) {
		return m
	}
	it := m.settingsItems[m.settingsCursor]
	switch it.kind {
	case settingPopup:
		if it.key == skLogFile {
			m.settingsStatus = "log file path is edited directly in config.toml"
			return m
		}
		m.showThemePicker = true
		m.syncThemeCursor()
		return m
	case settingToggle:
		if it.lspLang != "" {
			return m.toggleLSP(it)
		}
		return m.toggleBool(it.key, it.value == "on")
	case settingList:
		return m.openListEditor(it)
	case settingText:
		return m.openTextEditor(it)
	default:
		return m
	}
}

// toggleLSP flips a per-language [lsp.<lang>] enabled row and persists it in the
// current scope.
func (m Model) toggleLSP(it settingItem) Model {
	// An enabled language whose server is not installed displays as
	// "on (dormant)", so test the "on" prefix rather than equality — otherwise
	// toggling a dormant-but-enabled language would set enabled=true (a no-op)
	// instead of turning it off.
	v := !strings.HasPrefix(it.value, "on")
	if m.applyScopedLSP(it, v) {
		m.settingsStatus = m.scopedStatus(it.key, it.lspLang+" enabled "+onOff(v))
	}
	return m
}

// openListEditor opens the list-value editor for a settingList row, seeded with
// the effective list for the current scope. The row's lspLang (if any) is
// carried so commit persists to the right [lsp.<lang>] field.
func (m Model) openListEditor(it settingItem) Model {
	ed := newListEditor(it.key, it.label, m.effectiveList(it))
	ed.lspLang = it.lspLang
	m.settingsListEditor = ed
	return m
}

// handleListEditorKey routes a key to the open list editor. On commit it persists
// the entries to the current scope and pushes the appropriate reload.
func (m Model) handleListEditorKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.settingsListEditor == nil {
		return m, nil
	}
	if msg.String() == "ctrl+c" {
		return m.mainKeyQuit()
	}
	done, save := m.settingsListEditor.Update(msg)
	if !done {
		return m, nil
	}
	if save {
		return m.afterSettingChange(m.commitListEditor(), nil)
	}
	m.settingsListEditor = nil // esc — cancel, discard the edits
	return m, nil
}

// commitListEditor writes the editor's entries to the active scope and closes it.
func (m Model) commitListEditor() Model {
	ed := m.settingsListEditor
	m.settingsListEditor = nil
	if ed == nil {
		return m
	}
	entries := append([]string(nil), ed.entries...)
	if ed.lspLang != "" {
		if m.applyScopedLSP(settingItem{key: ed.key, lspLang: ed.lspLang}, entries) {
			m.settingsStatus = m.scopedStatus(ed.key, fmt.Sprintf("%s → %d entr%s", ed.title, len(entries), plural(len(entries))))
		}
		return m
	}
	apply := func(c *config.Config) {
		if p := listField(c, ed.key); p != nil {
			*p = entries
		}
	}
	if m.applyScopedSetting(ed.key, entries, apply) {
		m.settingsStatus = m.scopedStatus(ed.key, fmt.Sprintf("%s → %d entr%s", listLabel(ed.key), len(entries), plural(len(entries))))
	}
	return m
}

// openTextEditor opens the single-line text editor for a settingText row
// (currently the per-language [lsp.<lang>] command), seeded with the effective
// value for the current scope.
func (m Model) openTextEditor(it settingItem) Model {
	m.settingsTextEditor = newTextEditor(it.key, it.lspLang, it.label, m.effectiveText(it))
	return m
}

// handleTextEditorKey routes a key to the open text editor: enter saves, esc
// cancels (discards).
func (m Model) handleTextEditorKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.settingsTextEditor == nil {
		return m, nil
	}
	if msg.String() == "ctrl+c" {
		return m.mainKeyQuit()
	}
	done, save := m.settingsTextEditor.Update(msg)
	if !done {
		return m, nil
	}
	if save {
		return m.afterSettingChange(m.commitTextEditor(), nil)
	}
	m.settingsTextEditor = nil // esc — cancel, discard
	return m, nil
}

// commitTextEditor writes the editor's value to the active scope and closes it.
func (m Model) commitTextEditor() Model {
	ed := m.settingsTextEditor
	m.settingsTextEditor = nil
	if ed == nil {
		return m
	}
	val := strings.TrimSpace(ed.input)
	if ed.lspLang != "" {
		if m.applyScopedLSP(settingItem{key: ed.key, lspLang: ed.lspLang}, val) {
			m.settingsStatus = m.scopedStatus(ed.key, ed.title+" → "+pathOrDefault(val))
		}
		return m
	}
	apply := func(c *config.Config) {
		if p := stringField(c, ed.key); p != nil {
			*p = val
		}
	}
	if m.applyScopedSetting(ed.key, val, apply) {
		disp := pathOrDefault(val)
		if ed.key == skSemAPIKey {
			disp = maskedKey(val) // never echo the secret in the status line
		}
		m.settingsStatus = m.scopedStatus(ed.key, ed.title+" → "+disp)
	}
	return m
}

// effectiveText returns the string value for a settingText row in the current
// scope (currently only the per-language [lsp.<lang>] command).
func (m Model) effectiveText(it settingItem) string {
	cfg := m.settingsCfg
	if scope := m.currentScope(); !scope.global {
		if merged, err := config.LoadProject(m.settingsCfg, scope.folder); err == nil {
			cfg = merged
		}
	}
	if it.lspLang != "" && it.key == skLSPCommand {
		return cfg.LSP[it.lspLang].Command
	}
	if p := stringField(&cfg, it.key); p != nil {
		return *p
	}
	return ""
}

// stringField returns a pointer to the string config field a settingText row
// edits (the [semantics] text fields). Returns nil for non-string keys.
func stringField(c *config.Config, key settingKey) *string {
	switch key {
	case skSemModel:
		return &c.Semantics.Model
	case skSemBaseURL:
		return &c.Semantics.BaseURL
	case skSemAPIKeyEnv:
		return &c.Semantics.APIKeyEnv
	case skSemAPIKey:
		return &c.Semantics.APIKey
	default:
		return nil
	}
}

// effectiveList returns the list value for a row in the current scope: the
// merged project value in a workspace scope, the global snapshot in Global.
// Handles both the static list fields and the per-language [lsp.<lang>] lists.
func (m Model) effectiveList(it settingItem) []string {
	cfg := m.settingsCfg
	if scope := m.currentScope(); !scope.global {
		if merged, err := config.LoadProject(m.settingsCfg, scope.folder); err == nil {
			cfg = merged
		}
	}
	if it.lspLang != "" {
		e := cfg.LSP[it.lspLang]
		switch it.key {
		case skLSPArgs:
			return append([]string(nil), e.Args...)
		case skLSPRootMarkers:
			return append([]string(nil), e.RootMarkers...)
		}
		return nil
	}
	if p := listField(&cfg, it.key); p != nil {
		return append([]string(nil), (*p)...)
	}
	return nil
}

// listField returns a pointer to the []string config field a list row edits.
func listField(c *config.Config, key settingKey) *[]string {
	switch key {
	case skExtraRoots:
		return &c.Workspace.ExtraRoots
	case skReadRoots:
		return &c.Workspace.ReadRoots
	case skProtectedBranches:
		return &c.Git.ProtectedBranches
	case skExcludePatterns:
		return &c.Topology.ExcludePatterns
	case skAnalysers:
		return &c.Quality.Analysers
	default:
		return nil
	}
}

// listLabel is the human label for a list setting (editor title + status line).
func listLabel(key settingKey) string {
	switch key {
	case skExtraRoots:
		return "extra_roots"
	case skReadRoots:
		return "read_roots"
	case skProtectedBranches:
		return "protected_branches"
	case skExcludePatterns:
		return "exclude_patterns"
	case skAnalysers:
		return "analysers"
	default:
		return ""
	}
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
