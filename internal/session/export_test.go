package session

import "testing"

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
