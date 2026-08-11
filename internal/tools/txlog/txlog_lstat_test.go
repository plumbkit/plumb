package txlog

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRefuseSymlink_DistinguishesMissingFromUncheckable pins the difference
// between "I checked and it is not a link" and "I could not check".
//
// refuseSymlink returned nil for EVERY Lstat error while its comment claimed it
// was only forgiving a missing file. A permission denial on the parent
// directory therefore read as a clean bill of health for a path it had not
// inspected.
func TestRefuseSymlink_DistinguishesMissingFromUncheckable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a 0o000 directory is still traversable, so Lstat would not fail")
	}
	dir := t.TempDir()

	missing := filepath.Join(dir, "not-there")
	if err := refuseSymlink(missing); err != nil {
		t.Errorf("a missing path is not a symlink and must be admitted: %v", err)
	}

	regular := filepath.Join(dir, "plain.txt")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := refuseSymlink(regular); err != nil {
		t.Errorf("an ordinary file must be admitted: %v", err)
	}

	// An unreadable parent makes Lstat fail with something other than ENOENT.
	locked := filepath.Join(dir, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(locked, "target.txt")
	if err := os.WriteFile(inside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	if _, err := os.Lstat(inside); err == nil {
		t.Skip("this filesystem still permits Lstat through a 0o000 directory")
	}
	if err := refuseSymlink(inside); err == nil {
		t.Error("an Lstat failure that is not ENOENT must refuse: plumb cannot say whether " +
			"this path is a symlink, and \"could not check\" must not read as \"checked and fine\"")
	}
}
