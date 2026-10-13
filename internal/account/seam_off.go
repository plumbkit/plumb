//go:build !plumbtest

package account

import (
	"os"
	"testing"
)

// The seam WITHOUT the plumbtest tag: available only inside a test binary.
//
// testing.Testing() is a property of the binary, not of the environment: it is
// true when the process was built by `go test` and false in every shipped
// binary, so a release build has no way to reach this function's return value.
// That is the whole safety argument, and it is why the real seam (seam.go) adds
// a build tag on top: a CHILD process is not a test binary, so the smoke suites
// need the tag to give their children the same isolation.
func seamRoot() string {
	if !testing.Testing() {
		return ""
	}
	return ParseTestRoot(os.Getenv("PLUMB_TEST_ACCOUNT_ROOT"))
}

// authorityEnabled mirrors seamRoot's condition, and for the same reason: in a
// build without the tag there is no path by which a process that is not a test
// binary can turn the account authority on.
func authorityEnabled() bool {
	if !testing.Testing() {
		return false
	}
	return envTrue("PLUMB_ACCOUNT_AUTHORITY")
}
