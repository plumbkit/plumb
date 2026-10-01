package cli

import (
	"fmt"
	"os"
	"testing"

	"github.com/plumbkit/plumb/internal/session"
)

// TestMain moves the session registry to a temporary directory for the whole
// test binary. Tests in this package reach the registry, directly or through
// the code under test; without this they would take the user's real
// .sessions.lock and read or write the real session files, contending with the
// live daemon (#551). session.Dir ends a test binary that resolves to the live
// registry, so a missing override fails loudly rather than stalling.
//
// The directory is shared by every test in the binary, and PLUMB_SESSIONS_DIR
// outranks XDG_DATA_HOME, so a test's own t.Setenv("XDG_DATA_HOME", ...) does not
// give it a registry of its own. A test that needs an empty one sets
// t.Setenv(session.DirEnv, t.TempDir()).
func TestMain(m *testing.M) {
	os.Exit(runWithIsolatedSessions(m))
}

func runWithIsolatedSessions(m *testing.M) int {
	dir, err := os.MkdirTemp("", "plumb-sessions-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "TestMain: creating the session registry dir:", err)
		return 1
	}
	defer os.RemoveAll(dir) //nolint:errcheck // best-effort cleanup of a temp dir after the run
	if err := os.Setenv(session.DirEnv, dir); err != nil {
		fmt.Fprintln(os.Stderr, "TestMain: setting", session.DirEnv+":", err)
		return 1
	}
	return m.Run()
}
