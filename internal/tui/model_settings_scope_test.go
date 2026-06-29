package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/plumbkit/plumb/internal/config"
)

// TestTomlPath_ProjectVsGlobalOnly pins the single source of truth for which
// settings are project-overridable (have a TOML path) vs global-only.
func TestTomlPath_ProjectVsGlobalOnly(t *testing.T) {
	for _, k := range []settingKey{skStrict, skRateLimit, skGitPush, skTopoWatch, skQualityMode, skAllowDependencyReads} {
		if _, ok := tomlPath(k); !ok {
			t.Errorf("key %v should be project-overridable", k)
		}
	}
	for _, k := range []settingKey{skTheme, skLogLevel, skLogFormat, skCacheTTL, skLSPTimeout, skIdleThresholdMin} {
		if _, ok := tomlPath(k); ok {
			t.Errorf("key %v should be global-only (no project path)", k)
		}
	}
}

// TestBuildScopeItems_WorkspaceFiltersAndAnnotates verifies a workspace scope
// hides global-only rows and marks the keys present in the project file.
func TestBuildScopeItems_WorkspaceFiltersAndAnnotates(t *testing.T) {
	ws := t.TempDir()
	if err := config.SetProjectValue(ws, []string{"git", "allow_push"}, true); err != nil {
		t.Fatal(err)
	}
	m := &Model{
		settingsCfg:         config.Defaults(),
		settingsScopes:      []settingScope{{global: true, label: "Global"}, {folder: ws, label: "ws"}},
		settingsScopeCursor: 1,
	}
	items := m.buildScopeItems()
	if len(items) == 0 {
		t.Fatal("workspace scope produced no rows")
	}
	for _, it := range items {
		if _, ok := itemTOMLPath(it); !ok {
			t.Errorf("workspace scope leaked a global-only row: %v", it.key)
		}
	}
	var found bool
	for _, it := range items {
		switch it.key {
		case skGitPush:
			found = true
			if !it.overridden {
				t.Error("git allow_push should be marked overridden")
			}
		case skStrict:
			if it.overridden {
				t.Error("strict should be inherited, not overridden")
			}
		}
	}
	if !found {
		t.Error("git allow_push row missing from workspace scope")
	}
}

// TestApplyScopedSetting_WorkspaceWritesSparse verifies editing in a workspace
// scope writes only the touched key to the project file (and sets the project
// reload signal), and that reset removes it again.
func TestApplyScopedSetting_WorkspaceWritesSparse(t *testing.T) {
	ws := t.TempDir()
	m := Model{
		settingsCfg:         config.Defaults(),
		settingsScopes:      []settingScope{{global: true, label: "Global"}, {folder: ws, label: "ws"}},
		settingsScopeCursor: 1,
	}
	m.settingsItems = m.buildScopeItems()
	m.settingsCursor = cursorFor(m.settingsItems, skStrict)

	m = m.toggleBool(skStrict, false)
	if present, _ := config.ProjectValuePresent(ws, []string{"edits", "strict"}); !present {
		t.Error("toggling strict in a workspace scope should write edits.strict")
	}
	if m.pendingProjectReload != ws {
		t.Errorf("pendingProjectReload = %q, want %q", m.pendingProjectReload, ws)
	}
	// Global config must be untouched.
	if present, _ := config.ProjectValuePresent(ws, []string{"git", "allow_writes"}); present {
		t.Error("unrelated key git.allow_writes leaked into the project file")
	}

	m.settingsItems = m.buildScopeItems()
	m.settingsCursor = cursorFor(m.settingsItems, skStrict)
	m = m.resetToInherit()
	if present, _ := config.ProjectValuePresent(ws, []string{"edits", "strict"}); present {
		t.Error("resetToInherit should remove edits.strict")
	}
}

// TestListEditor_AddRemoveCommitWritesWorkspace exercises the list editor end to
// end in a workspace scope: open read_roots, add two entries, remove one, commit,
// and confirm only the surviving entry is written to the project file.
func TestListEditor_AddRemoveCommitWritesWorkspace(t *testing.T) {
	ws := t.TempDir()
	m := Model{
		settingsCfg:         config.Defaults(),
		settingsScopes:      []settingScope{{global: true, label: "Global"}, {folder: ws, label: "ws"}},
		settingsScopeCursor: 1,
	}
	m.settingsItems = m.buildScopeItems()
	m.settingsCursor = cursorFor(m.settingsItems, skExcludePatterns)
	if m.settingsCursor < 0 {
		t.Fatal("exclude_patterns row missing from workspace scope")
	}

	m = m.activateSetting()
	if m.settingsListEditor == nil {
		t.Fatal("activating exclude_patterns should open the list editor")
	}
	for _, entry := range []string{"/a", "/b"} {
		m.settingsListEditor.adding = true
		m.settingsListEditor.input = entry
		m.settingsListEditor.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	}
	if got := len(m.settingsListEditor.entries); got != 2 {
		t.Fatalf("entries after add = %d, want 2", got)
	}
	m.settingsListEditor.cursor = 0
	m.settingsListEditor.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyBackspace})) // remove selected entry
	m = m.commitListEditor()
	if m.settingsListEditor != nil {
		t.Error("commit should close the editor")
	}
	if m.pendingProjectReload != ws {
		t.Errorf("pendingProjectReload = %q, want %q", m.pendingProjectReload, ws)
	}
	merged, err := config.LoadProject(config.Defaults(), ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Topology.ExcludePatterns) != 1 || merged.Topology.ExcludePatterns[0] != "/b" {
		t.Errorf("exclude_patterns = %v, want [/b]", merged.Topology.ExcludePatterns)
	}
}

// TestListEditor_EscAutoSaves verifies esc closes the editor and auto-saves the
// in-memory entries (the editor no longer has a separate cancel/discard).
func TestListEditor_EscAutoSaves(t *testing.T) {
	ws := t.TempDir()
	m := Model{
		settingsCfg:         config.Defaults(),
		settingsScopes:      []settingScope{{global: true, label: "Global"}, {folder: ws, label: "ws"}},
		settingsScopeCursor: 1,
	}
	m.settingsItems = m.buildScopeItems()
	m.settingsCursor = cursorFor(m.settingsItems, skExcludePatterns)
	m = m.activateSetting()
	m.settingsListEditor.adding = true
	m.settingsListEditor.input = "/x"
	m.settingsListEditor.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter})) // add /x in-memory

	m, cmd := m.handleListEditorKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEsc}))
	if m.settingsListEditor != nil {
		t.Error("esc should close the editor")
	}
	if cmd == nil {
		t.Error("esc should auto-save and push a project reload")
	}
	merged, err := config.LoadProject(config.Defaults(), ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Topology.ExcludePatterns) != 1 || merged.Topology.ExcludePatterns[0] != "/x" {
		t.Errorf("exclude_patterns = %v, want [/x] (esc auto-saves)", merged.Topology.ExcludePatterns)
	}
}

// TestListEditor_EnterEditsInPlace verifies enter on a selected entry edits it
// in place rather than closing the editor.
func TestListEditor_EnterEditsInPlace(t *testing.T) {
	e := newListEditor(skReadRoots, "read_roots", []string{"/a", "/b"})
	e.cursor = 1
	if done, _ := e.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter})); done {
		t.Fatal("enter on an entry should edit in place, not close")
	}
	if !e.adding || !e.editing || e.input != "/b" {
		t.Fatalf("enter should load the entry for editing: adding=%v editing=%v input=%q", e.adding, e.editing, e.input)
	}
	e.input = "/c"
	e.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter})) // commit the edit
	if e.entries[1] != "/c" || len(e.entries) != 2 {
		t.Errorf("entries = %v, want [/a /c]", e.entries)
	}
}

// TestLSPRows_WorkspaceEditsWriteNestedKeys exercises the per-language
// [lsp.<lang>] rows in a workspace scope: the enable toggle and the command
// text editor each write only their nested key, and appear in the merged config.
func TestLSPRows_WorkspaceEditsWriteNestedKeys(t *testing.T) {
	ws := t.TempDir()
	m := Model{
		settingsCfg:         config.Defaults(),
		settingsScopes:      []settingScope{{global: true, label: "Global"}, {folder: ws, label: "ws"}},
		settingsScopeCursor: 1,
	}
	m.settingsItems = m.buildScopeItems()

	// Find the first per-language enable row.
	lang, enIdx := "", -1
	for i, it := range m.settingsItems {
		if it.lspLang != "" && it.key == skLSPEnabled {
			lang, enIdx = it.lspLang, i
			break
		}
	}
	if enIdx < 0 {
		t.Fatal("no per-language LSP enable row found")
	}

	// Toggle enabled → writes lsp.<lang>.enabled only.
	want := !m.settingsCfg.LSP[lang].Enabled
	m.settingsCursor = enIdx
	m = m.activateSetting()
	if present, _ := config.ProjectValuePresent(ws, []string{"lsp", lang, "enabled"}); !present {
		t.Errorf("toggling %s enabled should write lsp.%s.enabled", lang, lang)
	}
	if m.pendingProjectReload != ws {
		t.Errorf("pendingProjectReload = %q, want %q", m.pendingProjectReload, ws)
	}
	merged, err := config.LoadProject(config.Defaults(), ws)
	if err != nil {
		t.Fatal(err)
	}
	if merged.LSP[lang].Enabled != want {
		t.Errorf("merged lsp.%s.enabled = %v, want %v", lang, merged.LSP[lang].Enabled, want)
	}

	// Edit command via the text editor → writes lsp.<lang>.command only.
	m.settingsItems = m.buildScopeItems()
	cmdIdx := -1
	for i, it := range m.settingsItems {
		if it.lspLang == lang && it.key == skLSPCommand {
			cmdIdx = i
			break
		}
	}
	if cmdIdx < 0 {
		t.Fatalf("no command row for %s", lang)
	}
	m.settingsCursor = cmdIdx
	m = m.activateSetting()
	if m.settingsTextEditor == nil {
		t.Fatal("activating command should open the text editor")
	}
	m.settingsTextEditor.input = "/custom/bin/server"
	m = m.commitTextEditor()
	merged, _ = config.LoadProject(config.Defaults(), ws)
	if merged.LSP[lang].Command != "/custom/bin/server" {
		t.Errorf("merged lsp.%s.command = %q, want /custom/bin/server", lang, merged.LSP[lang].Command)
	}
}

// TestToggleLSP_DormantEnabledTurnsOff guards a regression in the per-language
// enable toggle: a language that is enabled but whose server is not installed
// displays as "on (dormant)", and toggling it must turn it OFF (enabled=false).
// The previous `it.value != "on"` test read "on (dormant)" as "not on" and set
// enabled back to true — a silent no-op. This is environment-independent because
// it pins the dormant display value directly rather than relying on whether the
// go server happens to be installed.
func TestToggleLSP_DormantEnabledTurnsOff(t *testing.T) {
	ws := t.TempDir()
	m := Model{
		settingsCfg:         config.Defaults(),
		settingsScopes:      []settingScope{{global: true, label: "Global"}, {folder: ws, label: "ws"}},
		settingsScopeCursor: 1,
	}
	it := settingItem{kind: settingToggle, key: skLSPEnabled, lspLang: "go", value: "on (dormant)"}
	m.toggleLSP(it) // persists via a sparse project-config write; return value unused

	present, err := config.ProjectValuePresent(ws, []string{"lsp", "go", "enabled"})
	if err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Fatal("toggling a dormant enable row should write lsp.go.enabled")
	}
	merged, err := config.LoadProject(config.Defaults(), ws)
	if err != nil {
		t.Fatal(err)
	}
	if merged.LSP["go"].Enabled {
		t.Error("toggling a dormant (enabled) language must set enabled=false; got true")
	}
}

// TestCollectSettingsScopes_GlobalFirst verifies Global leads the scope list and
// active workspaces follow.
func TestCollectSettingsScopes_GlobalFirst(t *testing.T) {
	m := &Model{dashProjectFolder: "/repo"}
	scopes := m.collectSettingsScopes()
	if len(scopes) < 2 || !scopes[0].global {
		t.Fatalf("first scope must be Global; got %+v", scopes)
	}
	if scopes[1].folder != "/repo" {
		t.Errorf("second scope folder = %q, want /repo", scopes[1].folder)
	}
}
