package session

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"

	"github.com/plumbkit/plumb/internal/paths"
)

// Dir returns the path to the session file directory, under plumb's data dir
// resolved by internal/paths (adrg/xdg). The error return is retained for API
// compatibility with callers; resolution does not fail.
//
// Inside a `go test` binary Dir ends the process rather than return the registry
// the user's own daemon uses (see testIsolationError and refuseLiveRegistry).
// Every registry operation resolves Dir before touching the filesystem, so that
// lands before the flock is taken or a file is written.
func Dir() (string, error) {
	dir := registryDir()
	if err := testIsolationError(dir, testBinary, liveDir); err != nil {
		refuseLiveRegistry(err)
		panic(err) // backstop: a handler that returns must still not hand back the live dir
	}
	return dir, nil
}

// registryDir is Dir without the test-binary guard.
func registryDir() string {
	return filepath.Join(paths.DataDir(), "sessions")
}

// testBinary reports whether this process is a `go test` binary. It is false
// in every production build, which is what keeps the guard below from ever
// firing outside tests.
var testBinary = testing.Testing()

// liveDir is the registry the environment this process started with resolves
// to — the registry a developer's own daemon, started from the same shell, is
// using. It is computed only in a test binary, before any TestMain or t.Setenv
// can move the data dir, and is empty in production.
//
// Read-only after package initialisation, so safe for concurrent use.
var liveDir = func() string {
	if !testBinary {
		return ""
	}
	return registryDir()
}()

// testIsolationError returns an error when a test binary is about to use the
// live session registry, and nil otherwise — always nil when testBinary is
// false.
//
// A test that reaches the registry without moving the data dir takes the real
// .sessions.lock and reads or writes the real session files, contending with
// the user's daemon (#551). Failing loudly here turns that from a slow, flaky,
// environment-dependent stall into a deterministic failure that names the fix.
// The check compares against the start-up environment rather than the user's
// home, so it fires identically on CI, where the start-up environment is just
// as live, and catches a new package that forgets its TestMain.
func testIsolationError(dir string, testBinary bool, live string) error {
	if !testBinary || live == "" || dir != live {
		return nil
	}
	return fmt.Errorf("session: a test resolved the session registry to %s, the live registry "+
		"this environment's plumb daemon uses; point XDG_DATA_HOME at a temporary directory in the "+
		"package's TestMain (or t.Setenv it to t.TempDir()) before touching sessions", dir)
}

// refuseLiveRegistry is how the guard ends a test binary that is about to use
// the live registry: it names the offender on stderr and exits.
//
// It exits rather than only panicking because the daemon's own recover() sites
// (the MCP dispatch, the per-connection goroutine) turn a panic into a logged
// error. A test that reached the registry through one of them would then pass
// with the guard having fired, which is the silent outcome the guard exists to
// prevent. The registry stays untouched either way, since Dir has not returned;
// what exiting adds is that the run fails. A variable only so a test can observe
// the guard in-process (export_test.go); the default is what a test binary runs.
var refuseLiveRegistry = func(err error) {
	fmt.Fprintf(os.Stderr, "FATAL: %v\n%s", err, debug.Stack())
	os.Exit(2)
}
