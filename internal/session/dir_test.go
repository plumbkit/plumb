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

// TestDir_FollowsXDGDataHome pins that the whole registry — the session files
// AND the .sessions.lock flock — lives under XDG_DATA_HOME, which is what lets a
// test (or a TestMain) move it off the user's real data directory.
func TestDir_FollowsXDGDataHome(t *testing.T) {
	xdgData := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgData)

	want := filepath.Join(xdgData, "plumb", "sessions")
	if dir, err := session.Dir(); err != nil || dir != want {
		t.Fatalf("Dir() = %q, %v; want %q", dir, err, want)
	}

	id, err := registerID(session.Info{Folder: "/tmp/x", Adapter: "gopls"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, name := range []string{id + ".json", ".sessions.lock"} {
		if _, err := os.Stat(filepath.Join(want, name)); err != nil {
			t.Errorf("%s not under XDG_DATA_HOME: %v", name, err)
		}
	}
}

// TestDir_RefusesLiveRegistryInTestBinary is the guard for #551: a test binary
// that resolves the registry to the one its start-up environment's daemon uses
// must be refused instead of taking the real flock. Two ways in are covered — a
// test that moved nothing, and one whose XDG_DATA_HOME is relative, which the
// XDG spec has implementations ignore, so the real default comes back. Only Dir
// is called, so a broken guard returns a path and fails the test without
// touching it.
//
// The default refusal ends the process, so this swaps it for a recorder; what
// that default does is TestDir_LiveRegistryEndsTheBinaryEvenIfTheCallerRecovers.
func TestDir_RefusesLiveRegistryInTestBinary(t *testing.T) {
	live := session.LiveDirForTest()
	if live == "" {
		t.Fatal("no live registry dir recorded in a test binary; the guard is disarmed")
	}
	// Each case is a mistake a test could make; neither moves the data dir off
	// the live registry. The value is the XDG_DATA_HOME to set, if any.
	cases := map[string]*string{
		"nothing moved":          nil,
		"relative XDG_DATA_HOME": ptr(filepath.Join("relative", "data")),
	}
	for name, xdg := range cases {
		t.Run(name, func(t *testing.T) {
			if xdg != nil {
				t.Setenv("XDG_DATA_HOME", *xdg)
			}
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
			if !strings.Contains(refused.Error(), "XDG_DATA_HOME") || !strings.Contains(refused.Error(), live) {
				t.Errorf("the refusal should name the live dir and XDG_DATA_HOME: %v", refused)
			}
			// A refusal that returns must still not hand the live dir back.
			if _, ok := got.(error); !ok {
				t.Errorf("Dir() after a returning refusal = %#v, want a panic carrying the error", got)
			}
		})
	}

	// Positive control: an isolated data dir resolves without a refusal.
	isolated := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolated)
	if dir, _ := session.Dir(); dir != filepath.Join(isolated, "plumb", "sessions") {
		t.Fatalf("Dir() = %q, want the registry under %q", dir, isolated)
	}
}

func ptr(s string) *string { return &s }

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
	cmd.Env = append(os.Environ(), childEnv+"=1")
	out, err := cmd.CombinedOutput()

	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("child err = %v, want it to exit non-zero; output:\n%s", err, out)
	}
	if strings.Contains(string(out), "CHILD-SURVIVED-THE-GUARD") {
		t.Errorf("the child got past a recovered call to Dir; output:\n%s", out)
	}
	if !strings.Contains(string(out), "XDG_DATA_HOME") {
		t.Errorf("the refusal should name XDG_DATA_HOME so the fix is clear; output:\n%s", out)
	}
}

// TestIsolationError_NeverFiresOutsideTestBinaries pins the production branch:
// with testBinary false the guard returns nil even for the live directory, so
// it cannot fire in a real plumb process. liveDir is also empty there, which
// TestDir_RefusesLiveRegistryInTestBinary's non-empty check contrasts.
func TestIsolationError_NeverFiresOutsideTestBinaries(t *testing.T) {
	const live = "/home/u/.local/share/plumb/sessions"
	if err := session.IsolationErrorForTest(live, false, live); err != nil {
		t.Errorf("production (testBinary=false): got %v, want nil", err)
	}
	if err := session.IsolationErrorForTest(live, true, ""); err != nil {
		t.Errorf("no live dir recorded: got %v, want nil", err)
	}
	if err := session.IsolationErrorForTest("/tmp/isolated", true, live); err != nil {
		t.Errorf("isolated dir in a test binary: got %v, want nil", err)
	}
	if err := session.IsolationErrorForTest(live, true, live); err == nil {
		t.Error("live dir in a test binary: got nil, want an error")
	}
}
