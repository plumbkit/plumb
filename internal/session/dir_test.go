package session_test

import (
	"errors"
	"os"
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

// TestDir_PanicsOnLiveRegistryInTestBinary is the guard for #551: a test binary
// that resolves the registry to the one its start-up environment's daemon uses
// must fail loudly instead of taking the real flock. Both ways in are covered —
// a test that moved nothing (override cleared, XDG as at start-up) and an
// override pointing straight at the live directory. Only Dir is called, so a
// broken guard returns a path and fails the test without touching it.
func TestDir_PanicsOnLiveRegistryInTestBinary(t *testing.T) {
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
			got := func() (r any) {
				defer func() { r = recover() }()
				dir, _ := session.Dir()
				t.Errorf("Dir() returned %q instead of panicking", dir)
				return nil
			}()
			err, ok := got.(error)
			if !ok {
				t.Fatalf("panic value = %#v, want an error", got)
			}
			if !strings.Contains(err.Error(), session.DirEnv) || !strings.Contains(err.Error(), live[0]) {
				t.Errorf("panic message should name the live dir and %s: %v", session.DirEnv, err)
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

// TestIsolationError_NeverFiresOutsideTestBinaries pins the production branch:
// with testBinary false the guard returns nil even for the live directory, so
// it cannot fire in a real plumb process. liveDirs is also nil there, which
// TestDir_PanicsOnLiveRegistryInTestBinary's non-empty check contrasts.
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
