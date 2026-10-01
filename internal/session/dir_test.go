package session_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/session"
)

// TestDir_HonoursSessionsDirOverride pins that PLUMB_SESSIONS_DIR moves the
// whole registry — the session files AND the .sessions.lock flock — and that
// XDG_DATA_HOME's location is left untouched while it is set. The positive
// control clears the override and confirms the same process then resolves to
// the XDG location, so the first half cannot pass because Dir ignored both.
func TestDir_HonoursSessionsDirOverride(t *testing.T) {
	override := t.TempDir()
	xdgData := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgData)
	t.Setenv(session.DirEnv, override)

	dir, err := session.Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if dir != override {
		t.Fatalf("Dir() = %q, want the override %q", dir, override)
	}

	id, err := registerID(session.Info{Folder: "/tmp/x", Adapter: "gopls"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, name := range []string{id + ".json", ".sessions.lock"} {
		if _, err := os.Stat(filepath.Join(override, name)); err != nil {
			t.Errorf("%s not in the override dir: %v", name, err)
		}
	}
	xdgSessions := filepath.Join(xdgData, "plumb", "sessions")
	if _, err := os.Stat(xdgSessions); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("XDG sessions dir %s was touched while the override was set (stat err %v)", xdgSessions, err)
	}

	// Positive control: without the override the same process resolves to XDG.
	t.Setenv(session.DirEnv, "")
	if dir, _ := session.Dir(); dir != xdgSessions {
		t.Fatalf("with the override cleared Dir() = %q, want %q", dir, xdgSessions)
	}
}

// TestDir_IgnoresRelativeOverride pins that a relative PLUMB_SESSIONS_DIR is
// ignored: the daemon and the CLI have different working directories, so
// honouring it would split the registry in two.
func TestDir_IgnoresRelativeOverride(t *testing.T) {
	xdgData := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgData)
	t.Setenv(session.DirEnv, filepath.Join("relative", "sessions"))

	want := filepath.Join(xdgData, "plumb", "sessions")
	if dir, _ := session.Dir(); dir != want {
		t.Fatalf("Dir() = %q, want the XDG default %q", dir, want)
	}
}

// TestDir_RefusesLiveRegistryInTestBinary is the guard for #551: a test binary
// that resolves the registry to the one its start-up environment's daemon uses
// must be refused instead of taking the real flock. Both ways in are covered —
// a test that moved nothing (override cleared, XDG as at start-up) and an
// override pointing straight at the live directory. Only Dir is called, so a
// broken guard returns a path and fails the test without touching it.
//
// The default refusal ends the process, so this swaps it for a recorder; what
// that default does is TestDir_LiveRegistryEndsTheBinaryEvenIfTheCallerRecovers.
func TestDir_RefusesLiveRegistryInTestBinary(t *testing.T) {
	live := session.LiveDirsForTest()
	if len(live) == 0 {
		t.Fatal("no live registry dirs recorded in a test binary; the guard is disarmed")
	}
	// Each case is the PLUMB_SESSIONS_DIR value the test runs with.
	cases := map[string]string{
		"nothing moved":            "",
		"override at the live dir": live[0],
	}
	for name, override := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(session.DirEnv, override)
			var refused error
			session.SetRefuseLiveRegistryForTest(t, func(err error) { refused = err })
			got := func() (r any) {
				defer func() { r = recover() }()
				dir, _ := session.Dir()
				t.Errorf("Dir() returned %q instead of refusing", dir)
				return nil
			}()
			if refused == nil {
				t.Fatal("the guard did not refuse the live registry")
			}
			if !strings.Contains(refused.Error(), session.DirEnv) || !strings.Contains(refused.Error(), live[0]) {
				t.Errorf("the refusal should name the live dir and %s: %v", session.DirEnv, refused)
			}
			// A refusal that returns must still not hand the live dir back.
			if _, ok := got.(error); !ok {
				t.Errorf("Dir() after a returning refusal = %#v, want a panic carrying the error", got)
			}
		})
	}

	// Positive control: an isolated registry resolves without panicking.
	isolated := t.TempDir()
	t.Setenv(session.DirEnv, isolated)
	if dir, _ := session.Dir(); dir != isolated {
		t.Fatalf("Dir() = %q, want %q", dir, isolated)
	}
}

// TestDir_LiveRegistryEndsTheBinaryEvenIfTheCallerRecovers pins the guard's
// default. The daemon's own recover() sites — the MCP dispatch and the
// per-connection goroutine — turn a panic into a logged error, so a test that
// reached the registry through one of them would pass with the guard having
// fired. Verified with a mutant that only panics: the child below survives and
// this fails.
//
// It re-executes this test binary as the child, which resolves the registry as
// its start-up environment does (nothing moved) and calls Dir inside a recover.
// Only Dir is called, so a broken guard returns a path and touches nothing.
func TestDir_LiveRegistryEndsTheBinaryEvenIfTheCallerRecovers(t *testing.T) {
	const childEnv = "PLUMB_SESSION_GUARD_CHILD"
	if os.Getenv(childEnv) == "1" {
		func() {
			defer func() { _ = recover() }()
			_, _ = session.Dir()
		}()
		fmt.Println("CHILD-SURVIVED-THE-GUARD")
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestDir_LiveRegistryEndsTheBinaryEvenIfTheCallerRecovers$", "-test.count=1")
	cmd.Env = append(os.Environ(), childEnv+"=1", session.DirEnv+"=")
	out, err := cmd.CombinedOutput()

	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("child err = %v, want it to exit non-zero; output:\n%s", err, out)
	}
	if strings.Contains(string(out), "CHILD-SURVIVED-THE-GUARD") {
		t.Errorf("the child got past a recovered call to Dir; output:\n%s", out)
	}
	if !strings.Contains(string(out), session.DirEnv) {
		t.Errorf("the refusal should name %s so the fix is clear; output:\n%s", session.DirEnv, out)
	}
}

// TestIsolationError_NeverFiresOutsideTestBinaries pins the production branch:
// with testBinary false the guard returns nil even for the live directory, so
// it cannot fire in a real plumb process. liveDirs is also nil there, which
// TestDir_RefusesLiveRegistryInTestBinary's non-empty check contrasts.
func TestIsolationError_NeverFiresOutsideTestBinaries(t *testing.T) {
	live := []string{"/home/u/.local/share/plumb/sessions"}
	if err := session.IsolationErrorForTest(live[0], false, live); err != nil {
		t.Errorf("production (testBinary=false): got %v, want nil", err)
	}
	if err := session.IsolationErrorForTest(live[0], true, nil); err != nil {
		t.Errorf("no live dirs recorded: got %v, want nil", err)
	}
	if err := session.IsolationErrorForTest("/tmp/isolated", true, live); err != nil {
		t.Errorf("isolated dir in a test binary: got %v, want nil", err)
	}
	if err := session.IsolationErrorForTest(live[0], true, live); err == nil {
		t.Error("live dir in a test binary: got nil, want an error")
	}
}
