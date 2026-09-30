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
