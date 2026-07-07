//go:build linux && integration

package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestSandbox_WriteJailEnforced_Bwrap is the Linux counterpart to the macOS
// jail-enforcement test: under a real bwrap, a command may write inside the
// workspace (with allow_writes) but a write outside it is refused. Gated behind
// the integration tag and skipped when bwrap is absent, so it runs on a Linux box
// that has bubblewrap installed (`go test -tags=integration ./internal/tools/`).
func TestSandbox_WriteJailEnforced_Bwrap(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not on PATH")
	}
	ws := t.TempDir()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	outside, err := os.MkdirTemp(home, ".plumb-sbtest-")
	if err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(outside) })

	run := func(opts SandboxOpts, script string) error {
		wrapped, st := sandboxWrap([]string{"/bin/sh", "-c", script}, opts)
		if !st.Active {
			t.Fatalf("sandbox inactive: %+v", st)
		}
		return exec.Command(wrapped[0], wrapped[1:]...).Run()
	}

	inFile := filepath.Join(ws, "ok.txt")
	if err := run(SandboxOpts{WorkspaceRoot: ws, AllowWrites: true}, "echo hi > "+inFile); err != nil {
		t.Fatalf("workspace write refused under bwrap despite allow_writes: %v", err)
	}
	if _, err := os.Stat(inFile); err != nil {
		t.Fatalf("workspace file not created under bwrap: %v", err)
	}

	outFile := filepath.Join(outside, "leak.txt")
	if err := run(SandboxOpts{WorkspaceRoot: ws, AllowWrites: true}, "echo pwned > "+outFile); err == nil {
		t.Error("write outside the workspace succeeded under bwrap — jail not enforced")
	}
	if _, err := os.Stat(outFile); err == nil {
		t.Errorf("file outside the workspace was created under bwrap: %s", outFile)
	}
}
