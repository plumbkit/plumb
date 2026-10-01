package session

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/paths"
)

// DirEnv names the environment variable that relocates the session registry.
// When it holds an absolute path, Dir returns that path verbatim instead of
// <data dir>/sessions. A relative value is ignored: the daemon and the CLI run
// from different working directories, so a relative registry would silently
// split in two.
//
// It exists so test binaries can move the registry — and its flock, which the
// live daemon takes on every Register, Patch and List — off the user's real
// data directory without also relocating everything else XDG_DATA_HOME moves.
// It is an environment variable rather than a package hook because the
// integration tests spawn plumb binaries and hook processes that must inherit
// the same registry.
const DirEnv = "PLUMB_SESSIONS_DIR"

// Dir returns the path to the session file directory: $PLUMB_SESSIONS_DIR when
// set to an absolute path, otherwise <data dir>/sessions resolved by
// internal/paths (adrg/xdg). The error return is retained for API compatibility
// with callers; resolution does not fail.
//
// Inside a `go test` binary Dir ends the process rather than return the registry
// the user's own daemon uses (see testIsolationError and refuseLiveRegistry).
// Every registry operation resolves Dir before touching the filesystem, so that
// lands before the flock is taken or a file is written.
func Dir() (string, error) {
	dir := resolveDir()
	if err := testIsolationError(dir, testBinary, liveDirs); err != nil {
		refuseLiveRegistry(err)
		panic(err) // backstop: a handler that returns must still not hand back the live dir
	}
	return dir, nil
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

// resolveDir is Dir without the test-binary guard.
func resolveDir() string {
	if dir := strings.TrimSpace(os.Getenv(DirEnv)); filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	return defaultDir()
}

// defaultDir is the registry location when DirEnv does not override it.
func defaultDir() string {
	return filepath.Join(paths.DataDir(), "sessions")
}

// testBinary reports whether this process is a `go test` binary. It is false
// in every production build, which is what keeps the guard below from ever
// firing outside tests.
var testBinary = testing.Testing()

// liveDirs are the registry locations the environment this process started with
// resolves to — the registry a developer's own daemon, started from the same
// shell, is using. It is computed only in a test binary, before any TestMain
// or t.Setenv can move the registry, and is nil in production.
//
// Both the default location and a pre-set DirEnv are recorded: a developer who
// exports PLUMB_SESSIONS_DIR for their daemon has made that directory the live
// one, and a test must not fall through to it either.
//
// Read-only after package initialisation, so safe for concurrent use.
var liveDirs = func() []string {
	if !testBinary {
		return nil
	}
	dirs := []string{defaultDir()}
	if dir := resolveDir(); dir != dirs[0] {
		dirs = append(dirs, dir)
	}
	return dirs
}()

// testIsolationError returns an error when a test binary is about to use the
// live session registry, and nil otherwise — always nil when testBinary is
// false.
//
// A test that reaches the registry without moving it takes the real
// .sessions.lock and reads or writes the real session files, contending with
// the user's daemon (#551). Failing loudly here turns that from a slow, flaky,
// environment-dependent stall into a deterministic failure that names the fix.
// The check compares against the start-up environment rather than the user's
// home, so it fires identically on CI, where the start-up environment is just
// as live, and catches a new package that forgets its TestMain.
func testIsolationError(dir string, testBinary bool, live []string) error {
	if !testBinary {
		return nil
	}
	for _, l := range live {
		if dir == l {
			return fmt.Errorf("session: a test resolved the session registry to %s, the live registry "+
				"this environment's plumb daemon uses; set %s to a temporary directory in the "+
				"package's TestMain (or t.Setenv XDG_DATA_HOME to t.TempDir()) before touching sessions",
				dir, DirEnv)
		}
	}
	return nil
}
