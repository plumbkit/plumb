package tui

import (
	"fmt"
	"os"
	"testing"
)

// TestMain moves the data directory to a temporary one for the whole test
// binary. Tests in this package reach the session registry, directly or through
// the code under test; without this, a test that does not set its own
// XDG_DATA_HOME would take the user's real .sessions.lock and read or write the
// real session files, contending with the live daemon (#551). session.Dir ends a
// test binary that still resolves to the live registry, so a package that
// forgets this fails loudly rather than stalling.
//
// It sets XDG_DATA_HOME rather than a dedicated registry override because a test
// that sets its own XDG_DATA_HOME must keep the private registry that gives it:
// the registry is where fixed session names, and the random draws that avoid
// them, are made unique, so one directory shared by every test lets a session
// another test left live hold a name this one asks for.
func TestMain(m *testing.M) {
	os.Exit(runWithIsolatedData(m))
}

func runWithIsolatedData(m *testing.M) int {
	dir, err := os.MkdirTemp("", "plumb-data-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "TestMain: creating the data dir:", err)
		return 1
	}
	defer os.RemoveAll(dir) //nolint:errcheck // best-effort cleanup of a temp dir after the run
	if err := os.Setenv("XDG_DATA_HOME", dir); err != nil {
		fmt.Fprintln(os.Stderr, "TestMain: setting XDG_DATA_HOME:", err)
		return 1
	}
	return m.Run()
}
