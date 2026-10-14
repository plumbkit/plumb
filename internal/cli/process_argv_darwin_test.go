//go:build darwin

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

// TestProcessArgv_KeepsASpaceInTheExecutablePath is the darwin half of the
// PLAN-552 review finding: `ps` flattened argv on whitespace, so a binary under a
// path with a space read as a different argv[0]. The kernel reader keeps it.
func TestProcessArgv_KeepsASpaceInTheExecutablePath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "with space")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A symlink, not a copy: a copied system binary loses its code signature and
	// macOS kills it at exec, leaving nothing to read. argv[0] is the path given.
	exe := filepath.Join(dir, "sleep")
	if err := os.Symlink("/bin/sleep", exe); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", exe, err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	argv, ok := processArgv(cmd.Process.Pid)
	if !ok || !reflect.DeepEqual(argv, []string{exe, "30"}) {
		t.Fatalf("processArgv = %q, %v; want [%q \"30\"]", argv, ok, exe)
	}
}
