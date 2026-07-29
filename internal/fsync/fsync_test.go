package fsync

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnabledDefaultsTrue(t *testing.T) {
	if !Enabled() {
		t.Error("Enabled should default to true when no function is installed")
	}
}

// TestSyncFile_DisabledSkipsSync proves the knob gates the sync: a Sync on a
// closed file always fails, so a nil return means the sync never happened.
func TestSyncFile_DisabledSkipsSync(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "f"))
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	SetEnabledFunc(func() bool { return false })
	t.Cleanup(func() { SetEnabledFunc(nil) })
	if err := SyncFile(f); err != nil {
		t.Errorf("knob off: SyncFile on closed file = %v, want nil (sync skipped)", err)
	}

	SetEnabledFunc(func() bool { return true })
	if err := SyncFile(f); err == nil {
		t.Error("knob on: SyncFile on closed file = nil, want error (sync attempted)")
	}
}

func TestSyncDir_DisabledSkipsError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	SetEnabledFunc(func() bool { return false })
	t.Cleanup(func() { SetEnabledFunc(nil) })
	if err := SyncDir(missing); err != nil {
		t.Errorf("knob off: SyncDir on missing dir = %v, want nil (sync skipped)", err)
	}

	SetEnabledFunc(func() bool { return true })
	if err := SyncDir(missing); err == nil {
		t.Error("knob on: SyncDir on missing dir = nil, want error")
	}
}

func TestSyncDir_RealDirSucceeds(t *testing.T) {
	if err := SyncDir(t.TempDir()); err != nil {
		t.Errorf("SyncDir on a real directory: %v", err)
	}
}
