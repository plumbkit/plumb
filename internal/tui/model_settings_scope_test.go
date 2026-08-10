package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/plumbkit/plumb/internal/config"
)

// TestTomlPath_ProjectVsGlobalOnly pins the single source of truth for which
// settings are project-overridable (have a TOML path) vs global-only.
func TestTomlPath_ProjectVsGlobalOnly(t *testing.T) {
	for _, k := range []settingKey{skStrict, skRateLimit, skTopoWatch, skQualityMode, skAllowDependencyReads} {
		if _, ok := tomlPath(k); !ok {
			t.Errorf("key %v should be project-overridable", k)
		}
	}
	for _, k := range []settingKey{skTheme, skLogLevel, skLogFormat, skCacheTTL, skLSPTimeout, skIdleThresholdMin} {
		if _, ok := tomlPath(k); ok {
			t.Errorf("key %v should be global-only (no project path)", k)
		}
	}
	// The [git] tier rows ARE project-overridable: LoadProject honours the block
	// for a trusted workspace. They were withdrawn while no project config could
	// ever have them honoured; the trust gate restored them. A row that cannot yet
	// take effect is marked notInEffect (see the workspace-scope test below), not
	// hidden — hiding it is what left a user unable to configure the setting at
	// all, and hiding it now would additionally hide a setting that IS live on a
	// trusted root.
	for _, k := range []settingKey{skGitWrites, skGitDestructive, skGitPush, skGitCommitTrailer, skProtectedBranches} {
		if _, ok := tomlPath(k); !ok {
			t.Errorf("git key %v should be project-overridable (honoured on a trusted workspace)", k)
		}
	}
}

// TestBuildScopeItems_WorkspaceFiltersAndAnnotates verifies a workspace scope
// hides global-only rows and marks the keys present in the project file, in the
// three states a workspace row can be in: a live override, inherited, and
// set-but-ignored.
//
// The third state is the one that matters. A capability-granting key ([git], an
// exec-deciding [lsp.<lang>] field) on an untrusted root is written to the
// project file and then disregarded by LoadProject; if that rendered as an
// ordinary override, the editor would be asserting a value plumb is not using —
// exactly the silent-no-op complaint the trust work exists to answer. It must
// report notInEffect, must NOT report overridden, and its value column must show
// what is actually in force.
func TestBuildScopeItems_WorkspaceFiltersAndAnnotates(t *testing.T) {
	ws := t.TempDir()
	if err := config.SetProjectValue(ws, []string{"topology", "watch"}, false); err != nil {
		t.Fatal(err)
	}
	// A capability-granting key on an untrusted root. allow_push defaults to
	// false, so "in effect" and "what the project asked for" differ visibly.
	if err := config.SetProjectValue(ws, []string{"git", "allow_push"}, true); err != nil {
		t.Fatal(err)
	}
	m := &Model{
		settingsCfg:         config.Defaults(),
		settingsScopes:      []settingScope{{global: true, label: "Global"}, {folder: ws, label: "ws"}},
		settingsScopeCursor: 1,
	}
	items := m.buildScopeItems()
	m.settingsItems = items
	if len(items) == 0 {
		t.Fatal("workspace scope produced no rows")
	}
	for _, it := range items {
		if _, ok := itemTOMLPath(it); !ok && !storeBackedWorkspaceKey(it.key) {
			t.Errorf("workspace scope leaked a global-only row: %v", it.key)
		}
	}
	var foundWatch, foundPush bool
	for _, it := range items {
		switch it.key {
		case skTopoWatch:
			foundWatch = true
			if !it.overridden || it.notInEffect {
				t.Error("topology watch is not capability-granting: it should be a live override")
			}
		case skStrict:
			if it.overridden || it.notInEffect {
				t.Error("strict should be inherited, not overridden")
			}
		case skGitPush:
			foundPush = true
			if !it.notInEffect {
				t.Error("git allow_push on an untrusted root must be marked NOT in effect")
			}
			if it.overridden {
				t.Error("an ignored project override must not also report as a live override")
			}
			if it.value != "off" {
				t.Errorf("git allow_push value = %q, want the global \"off\" that is actually in force", it.value)
			}
		case skGitWrites, skGitDestructive, skGitCommitTrailer, skProtectedBranches:
			// Present, and inherited: the project sets only allow_push.
			if it.notInEffect || it.overridden {
				t.Errorf("git row %v should be inherited here", it.key)
			}
		}
	}
	if !foundWatch {
		t.Error("topology watch row missing from workspace scope")
	}
	if !foundPush {
		t.Error("git allow_push row missing from workspace scope — the [git] tiers are project-overridable")
	}
	if !m.hasNotInEffectRow() {
		t.Error("hasNotInEffectRow should be true so the legend explains the ⁶ mark")
	}
}

// TestWorkspaceMark_ThreeStates pins the marker vocabulary each row state
// renders, since the mark is the user's only at-a-glance signal that a setting
// they wrote is not being used.
func TestWorkspaceMark_ThreeStates(t *testing.T) {
	cases := []struct {
		name string
		it   settingItem
		want string
	}{
		{"live override", settingItem{overridden: true}, "⁴"},
		{"inherited", settingItem{}, "⁵"},
		{"set but ignored", settingItem{notInEffect: true}, "⁶"},
	}
	for _, tc := range cases {
		if _, plain := workspaceMark(tc.it); plain != tc.want {
			t.Errorf("%s: mark = %q, want %q", tc.name, plain, tc.want)
		}
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

// TestWorkspaceRoots_EditsWriteStoreNotProject exercises the store-backed roots
// rows in a workspace scope: the row appears, committing the list editor writes
// the out-of-repo WorkspaceRootsStore (never the project config), sets the
// project-reload signal, and resetToInherit clears the grant.
func TestWorkspaceRoots_EditsWriteStoreNotProject(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()
	m := Model{
		settingsCfg:         config.Defaults(),
		settingsScopes:      []settingScope{{global: true, label: "Global"}, {folder: ws, label: "ws"}},
		settingsScopeCursor: 1,
	}
	m.settingsItems = m.buildScopeItems()
	m.settingsCursor = cursorFor(m.settingsItems, skExtraRoots)
	if m.settingsCursor < 0 {
		t.Fatal("extra_roots row missing from workspace scope")
	}

	m = m.activateSetting()
	if m.settingsListEditor == nil {
		t.Fatal("activating extra_roots should open the list editor")
	}
	m.settingsListEditor.adding = true
	m.settingsListEditor.input = "/data/shared"
	m.settingsListEditor.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	m = m.commitListEditor()

	if m.pendingProjectReload != ws {
		t.Errorf("pendingProjectReload = %q, want %q", m.pendingProjectReload, ws)
	}
	// The grant landed in the store, not the project config file.
	if got := config.NewWorkspaceRootsStore().Get(ws).ExtraRoots; len(got) != 1 || got[0] != "/data/shared" {
		t.Errorf("store extra roots = %v, want [/data/shared]", got)
	}
	if present, _ := config.ProjectValuePresent(ws, []string{"workspace", "extra_roots"}); present {
		t.Error("extra_roots must NOT be written to the project config file")
	}

	// The row now reflects the grant as an override.
	m.settingsItems = m.buildScopeItems()
	idx := cursorFor(m.settingsItems, skExtraRoots)
	if !m.settingsItems[idx].overridden {
		t.Error("a granted extra_roots row should be marked overridden")
	}

	// Reset clears the grant from the store.
	m.settingsCursor = idx
	m = m.resetToInherit()
	if got := config.NewWorkspaceRootsStore().Get(ws).ExtraRoots; len(got) != 0 {
		t.Errorf("resetToInherit should clear the store grant, got %v", got)
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
		settingsTab:         settingsTabLSP,
		settingsScopes:      []settingScope{{global: true, label: "Global"}, {folder: ws, label: "ws"}},
		settingsScopeCursor: 1,
	}
	m.refreshSettingsItems()

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
	//
	// command / args / root_markers ARE offered at a workspace scope: they decide
	// which process the daemon spawns and with what, so LoadProject honours them
	// only for a trusted root — but the row is meaningful, because trusting the
	// root makes it live. The untrusted state is not hidden, it is DECLARED: the
	// row comes back marked notInEffect with the value actually in force, and the
	// edit's own status line says the write will not take effect until
	// `plumb trust`. This is the UI half of the project-config trust boundary; the
	// config half is TestLoadProject_TrustIsBoundToContent.
	m.refreshSettingsItems()
	cmdIdx := -1
	for i, it := range m.settingsItems {
		if it.lspLang == lang && it.key == skLSPCommand {
			cmdIdx = i
			break
		}
	}
	if cmdIdx < 0 {
		t.Fatalf("no command row for %s — the exec-deciding LSP rows are workspace-editable", lang)
	}
	m.settingsCursor = cmdIdx
	m = m.activateSetting()
	if m.settingsTextEditor == nil {
		t.Fatal("activating command should open the text editor")
	}
	m.settingsTextEditor.input = "/custom/bin/server"
	m = m.commitTextEditor()

	if present, _ := config.ProjectValuePresent(ws, []string{"lsp", lang, "command"}); !present {
		t.Errorf("editing the command row should write lsp.%s.command", lang)
	}
	// Untrusted: the write lands in the file, and plumb keeps using the global
	// command. Both halves must be visible to the user.
	merged, _ = config.LoadProject(config.Defaults(), ws)
	if merged.LSP[lang].Command != config.Defaults().LSP[lang].Command {
		t.Errorf("merged lsp.%s.command = %q, want the global command (root is untrusted)",
			lang, merged.LSP[lang].Command)
	}
	var cmdRow settingItem
	for _, it := range m.settingsItems {
		if it.lspLang == lang && it.key == skLSPCommand {
			cmdRow = it
		}
	}
	if !cmdRow.notInEffect || cmdRow.overridden {
		t.Error("the edited command row must be marked NOT in effect on an untrusted root")
	}
	if cmdRow.value == "/custom/bin/server" {
		t.Error("the row shows the project file's value; it must show what is actually in effect")
	}
	if !strings.Contains(m.settingsStatus, "plumb trust") {
		t.Errorf("status = %q, want a message naming `plumb trust` — an edit that will not take "+
			"effect must not report plain success", m.settingsStatus)
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

// TestScopeRowState_FailsClosedOnUnreadableStatus pins the display's direction of
// failure. LoadProject fails closed when it cannot read trust state — it forces
// the capability sections back — so a row that presented as a live override would
// have the editor assert a value plumb is not using. An unreadable status must
// therefore render a capability row as ignored, and leave an ordinary row alone.
func TestScopeRowState_FailsClosedOnUnreadableStatus(t *testing.T) {
	boom := errors.New("unreadable")
	if overridden, notInEffect := scopeRowState(config.ProjectPolicyStatus{}, boom, []string{"lsp", "go", "command"}); overridden || !notInEffect {
		t.Error("an unreadable status must render a capability row as ignored, not live")
	}
	if overridden, notInEffect := scopeRowState(config.ProjectPolicyStatus{}, boom, []string{"git", "allow_push"}); overridden || !notInEffect {
		t.Error("an unreadable status must render a [git] row as ignored, not live")
	}
	if overridden, notInEffect := scopeRowState(config.ProjectPolicyStatus{}, boom, []string{"edits", "strict"}); !overridden || notInEffect {
		t.Error("a non-capability row is unaffected by trust and stays a plain override")
	}
}

// TestBuildScopeItems_UsesInjectedPolicyStatus verifies the rows read trust state
// through the package seam rather than the developer's real
// <DataDir>/trust.json, so the test's result does not depend on which workspaces
// the machine running it happens to have trusted.
func TestBuildScopeItems_UsesInjectedPolicyStatus(t *testing.T) {
	ws := t.TempDir()
	if err := config.SetProjectValue(ws, []string{"git", "allow_push"}, true); err != nil {
		t.Fatal(err)
	}
	prev := projectPolicyStatus
	t.Cleanup(func() { projectPolicyStatus = prev })
	projectPolicyStatus = func(string) (config.ProjectPolicyStatus, error) {
		return config.ProjectPolicyStatus{
			Spec:    config.ProjectPolicySpec{{Key: "git.allow_push", Value: true}},
			Trusted: true,
		}, nil
	}
	m := &Model{
		settingsCfg:         config.Defaults(),
		settingsScopes:      []settingScope{{global: true, label: "Global"}, {folder: ws, label: "ws"}},
		settingsScopeCursor: 1,
	}
	for _, it := range m.buildScopeItems() {
		if it.key == skGitPush {
			if !it.overridden || it.notInEffect {
				t.Error("a TRUSTED capability key must render as a live override")
			}
			return
		}
	}
	t.Error("git allow_push row missing")
}
