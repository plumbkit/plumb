package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/golimpio/plumb/internal/config"
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
		if _, ok := tomlPath(it.key); !ok {
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
	m.settingsCursor = cursorFor(m.settingsItems, skReadRoots)
	if m.settingsCursor < 0 {
		t.Fatal("read_roots row missing from workspace scope")
	}

	m = m.activateSetting()
	if m.settingsListEditor == nil {
		t.Fatal("activating read_roots should open the list editor")
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
	if len(merged.Workspace.ReadRoots) != 1 || merged.Workspace.ReadRoots[0] != "/b" {
		t.Errorf("read_roots = %v, want [/b]", merged.Workspace.ReadRoots)
	}
}

// TestListEditor_EscCancelsDiscards verifies esc closes the editor without
// persisting (the conventional cancel) and without triggering a reload.
func TestListEditor_EscCancelsDiscards(t *testing.T) {
	ws := t.TempDir()
	m := Model{
		settingsCfg:         config.Defaults(),
		settingsScopes:      []settingScope{{global: true, label: "Global"}, {folder: ws, label: "ws"}},
		settingsScopeCursor: 1,
	}
	m.settingsItems = m.buildScopeItems()
	m.settingsCursor = cursorFor(m.settingsItems, skReadRoots)
	m = m.activateSetting()
	m.settingsListEditor.adding = true
	m.settingsListEditor.input = "/x"
	m.settingsListEditor.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter})) // add /x in-memory

	m, _ = m.handleListEditorKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEsc}))
	if m.settingsListEditor != nil {
		t.Error("esc should close the editor")
	}
	if m.pendingProjectReload != "" {
		t.Error("esc cancel must not trigger a project reload")
	}
	if present, _ := config.ProjectValuePresent(ws, []string{"workspace", "read_roots"}); present {
		t.Error("esc cancel must not write read_roots")
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
