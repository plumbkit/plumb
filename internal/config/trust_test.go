package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTrustStore_DefaultUntrusted(t *testing.T) {
	s := newTrustStoreAt(filepath.Join(t.TempDir(), "trust.json"))
	if s.IsTrusted("/some/workspace") {
		t.Error("a never-trusted root should be untrusted")
	}
}

func TestTrustStore_PersistsPerRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trust.json")
	s := newTrustStoreAt(path)
	root := t.TempDir()
	if err := s.SetTrusted(root, true); err != nil {
		t.Fatalf("SetTrusted: %v", err)
	}
	// A fresh store reading the same file sees the trust — it persisted.
	if !newTrustStoreAt(path).IsTrusted(root) {
		t.Error("trust did not persist across store instances")
	}
	// A different root is unaffected.
	if s.IsTrusted(t.TempDir()) {
		t.Error("trusting one root must not trust another")
	}
}

func TestTrustStore_Revoke(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trust.json")
	s := newTrustStoreAt(path)
	root := t.TempDir()
	_ = s.SetTrusted(root, true)
	if err := s.SetTrusted(root, false); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if s.IsTrusted(root) {
		t.Error("trust should be cleared after SetTrusted(false)")
	}
}

// TestTrustStore_NotWrittenIntoProject is the security invariant: trust lives in
// plumb's data dir, never inside the workspace, so a cloned repo cannot ship its
// own trust.
func TestTrustStore_NotWrittenIntoProject(t *testing.T) {
	dataDir := t.TempDir()
	workspace := t.TempDir()
	s := newTrustStoreAt(filepath.Join(dataDir, "trust.json"))
	if err := s.SetTrusted(workspace, true); err != nil {
		t.Fatalf("SetTrusted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "trust.json")); !os.IsNotExist(err) {
		t.Error("trust must not be written into the workspace")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "trust.json")); err != nil {
		t.Errorf("trust file should live in the data dir: %v", err)
	}
}
