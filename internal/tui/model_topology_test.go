package tui

import (
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/session"
)

func TestTopologyDetailRow_OmittedWhenNoIndex(t *testing.T) {
	var m Model // topoStatusOK defaults to false
	if _, ok := m.topologyDetailRow(); ok {
		t.Error("expected no topology row when topoStatusOK is false")
	}
}

func TestTopologyDetailRow_PresentWhenIndexed(t *testing.T) {
	RebuildStyles()
	m := Model{topoStatusOK: true}
	m.topoStatus.TotalNodes = 1234
	m.topoStatus.IndexedFiles = 56
	m.topoStatus.Languages = []string{"go"}
	row, ok := m.topologyDetailRow()
	if !ok {
		t.Fatal("expected a topology row when topoStatusOK is true")
	}
	if row == "" {
		t.Error("expected non-empty topology row")
	}
}

// TestRefreshTopology_DebouncesWithinInterval verifies a recent read for the
// same workspace is not repeated: the cached status survives the call. The
// workspace has no on-disk index, so a re-read would clear topoStatusOK — the
// fact that it stays true proves no read happened.
func TestRefreshTopology_DebouncesWithinInterval(t *testing.T) {
	dir := t.TempDir() // no .plumb/topology.db present
	m := Model{
		sessions:         []session.Info{{Folder: dir}},
		topoStatusFolder: dir,
		topoStatusAt:     time.Now(),
		topoStatusOK:     true, // pretend a prior read succeeded
	}
	m.refreshTopology()
	if !m.topoStatusOK {
		t.Error("expected the cached status to be preserved within the debounce interval (no re-read)")
	}
}

// TestRefreshTopology_RefetchesOnFolderChange verifies a change of the selected
// workspace forces an immediate re-read, bypassing the debounce window.
func TestRefreshTopology_RefetchesOnFolderChange(t *testing.T) {
	dir := t.TempDir() // no index at the new folder
	m := Model{
		sessions:         []session.Info{{Folder: dir}},
		topoStatusFolder: "/some/other/workspace",
		topoStatusAt:     time.Now(),
		topoStatusOK:     true, // stale cached value for the previous folder
	}
	m.refreshTopology()
	if m.topoStatusOK {
		t.Error("expected a re-read on workspace change to clear the stale status (no index at the new folder)")
	}
	if m.topoStatusFolder != dir {
		t.Errorf("topoStatusFolder = %q, want %q", m.topoStatusFolder, dir)
	}
}
