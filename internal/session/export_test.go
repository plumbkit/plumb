package session

import (
	"os"
	"sync/atomic"
	"testing"
)

// export_test.go exposes internals to the external session_test package. It is
// an _test.go file, so none of this is compiled into the package's real API.

// SetGenerateNameForTest replaces the random name draw for the duration of the
// test and restores it afterwards.
//
// Forcing a constant draw is the only way to reach the collision and suffix
// paths: the pool is a few thousand names, so a real draw essentially never
// collides, and the code that handles it when it does would otherwise be
// unexercised.
func SetGenerateNameForTest(t *testing.T, fn func() string) {
	t.Helper()
	orig := generateName
	generateName = fn
	t.Cleanup(func() { generateName = orig })
}

// LiveDirForTest returns the registry location the test binary's start-up
// environment resolves to, which Dir refuses to return inside a test.
func LiveDirForTest() string { return liveDir }

// IsolationErrorForTest exposes the guard's decision so its production
// branch (testBinary false) can be exercised from a test binary.
func IsolationErrorForTest(dir string, testBinary bool, live string) error {
	return testIsolationError(dir, testBinary, live)
}

// SetRefuseLiveRegistryForTest replaces what the guard does when Dir resolves
// the live registry, for the duration of the test. The default ends the whole
// test binary, so without this a test could not see the guard fire in-process;
// the default itself is covered by re-executing the test binary.
func SetRefuseLiveRegistryForTest(t *testing.T, fn func(error)) {
	t.Helper()
	orig := refuseLiveRegistry
	refuseLiveRegistry = fn
	t.Cleanup(func() { refuseLiveRegistry = orig })
}

// EndedSessionGraceForTest is how long an ended session's file is kept.
const EndedSessionGraceForTest = endedSessionGrace

// CountSessionFileReadsForTest counts every session file List reads for the
// rest of the test. A read count is what separates "remembered" from "re-read"
// deterministically; a timing assertion on the same thing would flake.
func CountSessionFileReadsForTest(t *testing.T) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	orig := readSessionFile
	readSessionFile = func(name string) ([]byte, error) {
		n.Add(1)
		return os.ReadFile(name) //nolint:gosec // G304: test seam over the session directory
	}
	t.Cleanup(func() { readSessionFile = orig })
	return &n
}
