package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkspaceRootsStore_DefaultEmpty(t *testing.T) {
	s := newWorkspaceRootsStoreAt(filepath.Join(t.TempDir(), "workspace_roots.json"))
	wr := s.Get("/some/workspace")
	if len(wr.ExtraRoots) != 0 || len(wr.ReadRoots) != 0 {
		t.Errorf("a never-granted root should be empty, got %+v", wr)
	}
}

func TestWorkspaceRootsStore_PersistsPerRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspace_roots.json")
	s := newWorkspaceRootsStoreAt(path)
	root := t.TempDir()
	if err := s.SetExtraRoots(root, []string{"/data/shared"}); err != nil {
		t.Fatalf("SetExtraRoots: %v", err)
	}
	if err := s.SetReadRoots(root, []string{"/ref/docs"}); err != nil {
		t.Fatalf("SetReadRoots: %v", err)
	}
	// A fresh store reading the same file sees the grant — it persisted.
	fresh := newWorkspaceRootsStoreAt(path).Get(root)
	if len(fresh.ExtraRoots) != 1 || fresh.ExtraRoots[0] != "/data/shared" {
		t.Errorf("extra roots did not persist: %+v", fresh.ExtraRoots)
	}
	if len(fresh.ReadRoots) != 1 || fresh.ReadRoots[0] != "/ref/docs" {
		t.Errorf("read roots did not persist: %+v", fresh.ReadRoots)
	}
	// A different root is unaffected.
	if other := s.Get(t.TempDir()); len(other.ExtraRoots) != 0 || len(other.ReadRoots) != 0 {
		t.Errorf("granting one root must not affect another: %+v", other)
	}
}

func TestWorkspaceRootsStore_ClearPrunesEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspace_roots.json")
	s := newWorkspaceRootsStoreAt(path)
	root := t.TempDir()
	if err := s.SetExtraRoots(root, []string{"/data/shared"}); err != nil {
		t.Fatalf("SetExtraRoots: %v", err)
	}
	if err := s.SetExtraRoots(root, nil); err != nil {
		t.Fatalf("clear SetExtraRoots: %v", err)
	}
	if wr := s.Get(root); len(wr.ExtraRoots) != 0 {
		t.Errorf("extra roots should be cleared, got %+v", wr.ExtraRoots)
	}
	// The now-empty entry is pruned, so the file is an empty object.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading store: %v", err)
	}
	if got := string(data); got != "{}" {
		t.Errorf("empty store should be {}, got %q", got)
	}
}

func TestWorkspaceRootsStore_SetExtraKeepsReadRoots(t *testing.T) {
	s := newWorkspaceRootsStoreAt(filepath.Join(t.TempDir(), "workspace_roots.json"))
	root := t.TempDir()
	_ = s.SetReadRoots(root, []string{"/ref"})
	_ = s.SetExtraRoots(root, []string{"/rw"})
	// Clearing extra roots must leave the read roots intact.
	_ = s.SetExtraRoots(root, nil)
	if wr := s.Get(root); len(wr.ReadRoots) != 1 || wr.ReadRoots[0] != "/ref" {
		t.Errorf("read roots should survive clearing extra roots: %+v", wr)
	}
}

func TestWorkspaceRootsStore_BlanksDropped(t *testing.T) {
	s := newWorkspaceRootsStoreAt(filepath.Join(t.TempDir(), "workspace_roots.json"))
	root := t.TempDir()
	_ = s.SetExtraRoots(root, []string{"", "/keep", ""})
	if wr := s.Get(root); len(wr.ExtraRoots) != 1 || wr.ExtraRoots[0] != "/keep" {
		t.Errorf("blank entries should be dropped: %+v", wr.ExtraRoots)
	}
}

func TestWorkspaceRootsStore_ReadErrorFailsClosed(t *testing.T) {
	// A path whose parent is a file (not a dir) makes ReadFile return a non
	// IsNotExist error; Get must fail closed to an empty grant, never widen.
	base := t.TempDir()
	notADir := filepath.Join(base, "afile")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newWorkspaceRootsStoreAt(filepath.Join(notADir, "workspace_roots.json"))
	if wr := s.Get("/some/workspace"); len(wr.ExtraRoots) != 0 || len(wr.ReadRoots) != 0 {
		t.Errorf("a read error must fail closed to an empty grant, got %+v", wr)
	}
}

// TestWorkspaceRootsStore_NotWrittenIntoProject is the security invariant: grants
// live in plumb's data dir, never inside the workspace, so a cloned repo cannot
// ship its own roots.
func TestWorkspaceRootsStore_NotWrittenIntoProject(t *testing.T) {
	dataDir := t.TempDir()
	workspace := t.TempDir()
	s := newWorkspaceRootsStoreAt(filepath.Join(dataDir, "workspace_roots.json"))
	if err := s.SetExtraRoots(workspace, []string{"/data/shared"}); err != nil {
		t.Fatalf("SetExtraRoots: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "workspace_roots.json")); !os.IsNotExist(err) {
		t.Error("grants must not be written into the workspace")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "workspace_roots.json")); err != nil {
		t.Errorf("grants should live in the data dir: %v", err)
	}
}
