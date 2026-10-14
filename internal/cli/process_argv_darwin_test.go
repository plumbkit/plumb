//go:build darwin

package cli

import (
	"io"
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
	exe := filepath.Join(dir, "sleep")
	copyFile(t, "/bin/sleep", exe)
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

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	src, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	dst, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY, 0o700) //nolint:gosec // G302: an executable test fixture
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
}
