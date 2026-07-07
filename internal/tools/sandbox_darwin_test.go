//go:build darwin

package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildSeatbeltProfile(t *testing.T) {
	// Always present: the permissive default, the blanket write denial.
	base := buildSeatbeltProfile(false, false)
	if !strings.Contains(base, "(deny file-write* (subpath (param \"PLUMBDIR\")))") {
		t.Errorf("profile does not re-deny writes to plumb's runtime dir:\n%s", base)
	}
	for _, want := range []string{"(allow default)", "(deny file-write*)", "(subpath (param \"TMP\"))"} {
		if !strings.Contains(base, want) {
			t.Errorf("profile missing %q:\n%s", want, base)
		}
	}
	if strings.Contains(base, "(param \"WS\")") {
		t.Error("profile granted workspace writes when allowWrites=false")
	}
	if strings.Contains(base, "(deny network*)") {
		t.Error("profile denied network when denyNetwork=false")
	}
	full := buildSeatbeltProfile(true, true)
	if !strings.Contains(full, "(subpath (param \"WS\"))") {
		t.Error("profile did not grant workspace writes when allowWrites=true")
	}
	if !strings.Contains(full, "(deny network*)") {
		t.Error("profile did not deny network when denyNetwork=true")
	}
}

func TestSandboxWrap_Shape(t *testing.T) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("sandbox-exec not on PATH")
	}
	argv := []string{"go", "build", "./..."}
	wrapped, st := sandboxWrap(argv, SandboxOpts{WorkspaceRoot: "/ws", AllowWrites: true})
	if !st.Active || st.Mechanism != "sandbox-exec" {
		t.Fatalf("status = %+v, want active sandbox-exec", st)
	}
	if !strings.HasSuffix(wrapped[0], "sandbox-exec") {
		t.Fatalf("wrapped[0] = %q, want sandbox-exec", wrapped[0])
	}
	// The original argv must be the tail, unmodified.
	if got := wrapped[len(wrapped)-3:]; got[0] != "go" || got[1] != "build" || got[2] != "./..." {
		t.Fatalf("original argv not preserved at tail: %v", wrapped)
	}
}

// TestSandbox_WriteJailEnforced is a real, hermetic enforcement test: a command
// run under the sandbox may write inside the workspace (with allow_writes) but a
// write outside it is refused by the kernel. Proves the jail actually holds on
// macOS, not just that the argv looks right.
func TestSandbox_WriteJailEnforced(t *testing.T) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("sandbox-exec not on PATH")
	}
	ws := t.TempDir()
	// t.TempDir() lives under $TMPDIR, which the jail deliberately allows — so an
	// "outside" location must sit elsewhere. A dir directly under $HOME (not the
	// cache/module subtrees) is user-writable normally but denied by the jail.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	outside, err := os.MkdirTemp(home, ".plumb-sbtest-")
	if err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(outside) })

	runShell := func(opts SandboxOpts, script string) error {
		wrapped, st := sandboxWrap([]string{"/bin/sh", "-c", script}, opts)
		if !st.Active {
			t.Fatalf("sandbox inactive: %+v", st)
		}
		cmd := exec.Command(wrapped[0], wrapped[1:]...)
		return cmd.Run()
	}

	// Writing inside the workspace with allow_writes succeeds.
	inFile := filepath.Join(ws, "ok.txt")
	if err := runShell(SandboxOpts{WorkspaceRoot: ws, AllowWrites: true}, "echo hi > "+inFile); err != nil {
		t.Fatalf("workspace write refused despite allow_writes: %v", err)
	}
	if _, err := os.Stat(inFile); err != nil {
		t.Fatalf("workspace file not created: %v", err)
	}

	// Writing outside the workspace is refused (the file must not appear).
	outFile := filepath.Join(outside, "leak.txt")
	if err := runShell(SandboxOpts{WorkspaceRoot: ws, AllowWrites: true}, "echo pwned > "+outFile); err == nil {
		t.Error("write outside the workspace succeeded — jail not enforced")
	}
	if _, err := os.Stat(outFile); err == nil {
		t.Errorf("file outside the workspace was created: %s", outFile)
	}
}

func TestPlumbRuntimeDir(t *testing.T) {
	d := plumbRuntimeDir()
	if filepath.Base(d) != "plumb" {
		t.Errorf("plumbRuntimeDir() = %q, want a .../plumb path", d)
	}
	if cache, err := os.UserCacheDir(); err == nil {
		if want := filepath.Join(cache, "plumb"); d != want {
			t.Errorf("plumbRuntimeDir() = %q, want %q", d, want)
		}
	}
}

// TestSandbox_DenyNetworkEnforced proves deny_network actually cuts the network,
// not just that the profile carries the rule. Skips gracefully when there is no
// sandbox, no nc, or no outbound network (so it never flakes in a sealed CI box).
func TestSandbox_DenyNetworkEnforced(t *testing.T) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("sandbox-exec not on PATH")
	}
	nc, err := exec.LookPath("nc")
	if err != nil {
		t.Skip("nc not on PATH")
	}
	ws := t.TempDir()
	dial := []string{nc, "-G2", "-w2", "-z", "1.1.1.1", "53"}

	// Baseline: without deny_network the dial should connect. If it can't (no
	// network here), we can't meaningfully assert the deny — skip.
	openWrap, _ := sandboxWrap(dial, SandboxOpts{WorkspaceRoot: ws})
	if err := exec.Command(openWrap[0], openWrap[1:]...).Run(); err != nil {
		t.Skipf("no outbound network available: %v", err)
	}

	// With deny_network the same dial must fail.
	denyWrap, st := sandboxWrap(dial, SandboxOpts{WorkspaceRoot: ws, DenyNetwork: true})
	if !st.Active {
		t.Fatalf("sandbox inactive: %+v", st)
	}
	if err := exec.Command(denyWrap[0], denyWrap[1:]...).Run(); err == nil {
		t.Error("dial succeeded under deny_network — the network was not blocked")
	}
}
