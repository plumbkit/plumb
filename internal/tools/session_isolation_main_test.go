package tools

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
// live daemon (#551). session.Dir panics in a test binary that resolves to the
// live registry, so a missing override fails loudly rather than stalling.
//
// The directory is shared by every test in the binary. A test that needs an
// empty registry of its own sets t.Setenv(session.DirEnv, t.TempDir()).
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
