package tui

// model_settings_roots.go — routing for the two store-backed workspace roots
// rows (Extra roots / Read roots). In a WORKSPACE scope these edit the
// out-of-repo config.WorkspaceRootsStore (a manual, trusted grant recorded in
// plumb's data dir) rather than the project's .plumb/config.toml — a project
// config cannot widen its own filesystem access (LoadProject forces its
// extra_roots/read_roots back to the global base). In the GLOBAL scope these
// keys are ordinary config.Workspace.{ExtraRoots,ReadRoots} rows, handled by the
// generic list path.

import (
	"fmt"

	"github.com/plumbkit/plumb/internal/config"
)

// storeBackedWorkspaceKey reports whether a settings key is a per-workspace root
// grant that, in a workspace scope, is persisted to the WorkspaceRootsStore
// instead of the project config file.
func storeBackedWorkspaceKey(key settingKey) bool {
	return key == skExtraRoots || key == skReadRoots
}

// workspaceRootsFor returns the roots the store grants folder for a store-backed
// key. Used to populate the row value/list and to pre-fill the list editor.
func workspaceRootsFor(folder string, key settingKey) []string {
	if folder == "" {
		return nil
	}
	wr := config.NewWorkspaceRootsStore().Get(folder)
	switch key {
	case skExtraRoots:
		return append([]string(nil), wr.ExtraRoots...)
	case skReadRoots:
		return append([]string(nil), wr.ReadRoots...)
	default:
		return nil
	}
}

// applyStoreRoots overrides a store-backed row's value/list/overridden from the
// per-workspace store, so a workspace scope shows the granted roots (not the
// forced-to-base global value that buildSettingItems put there).
func applyStoreRoots(it settingItem, folder string) settingItem {
	roots := workspaceRootsFor(folder, it.key)
	it.list = roots
	it.value = listSummary(roots)
	it.overridden = len(roots) > 0
	return it
}

// writeWorkspaceRoots persists entries for a store-backed key in the current
// workspace scope, then reuses the reload-project plumbing (pendingProjectReload)
// so the live daemon rebuilds its PathPolicy and re-reads the store. An empty
// list clears the grant. Callers set the status line. Returns true on success.
func (m *Model) writeWorkspaceRoots(key settingKey, entries []string) bool {
	folder := m.currentScope().folder
	if folder == "" {
		return false
	}
	store := config.NewWorkspaceRootsStore()
	var err error
	switch key {
	case skExtraRoots:
		err = store.SetExtraRoots(folder, entries)
	case skReadRoots:
		err = store.SetReadRoots(folder, entries)
	default:
		return false
	}
	if err != nil {
		m.settingsStatus = "save failed: " + err.Error()
		return false
	}
	m.pendingProjectReload = folder
	m.refreshSettingsItems()
	return true
}

// workspaceRootsStatus is the transient status shown after a store-backed roots
// edit — worded as a workspace grant, distinct from a project-config override.
func workspaceRootsStatus(key settingKey, n int) string {
	return fmt.Sprintf("%s → %d entr%s · workspace grant", listLabel(key), n, plural(n))
}
