//go:build plumbtest

package account

import (
	"os"
	"testing"

	"github.com/plumbkit/plumb/internal/separateenv"
)

// The test seam, compiled in ONLY with `-tags=plumbtest`.
//
// Two independent conditions guard it, because this is the one path by which a
// process can make account resolution point somewhere other than the account:
//
//   - in-process (a test binary, where testing.Testing() is true) the seam needs
//     only PLUMB_TEST_ACCOUNT_ROOT, so `go test ./internal/...` can isolate;
//   - out of process (a real child binary built with the tag, as cmd/smoke and
//     cmd/clientsmoke build theirs) it additionally requires separateenv evidence
//     — CI, a readable container marker, or a declared VM. A tagged binary on a
//     developer's machine therefore still cannot take the seam, which is what
//     stops a test from reaching the owner's live daemon.
//
// Release builds pass no such tag, so in them this file does not exist at all
// and seam_off.go's answer — the empty string — is the only one available.
// authorityEnabled is the gate itself, and it is the same two conditions as the
// seam root: a tagged CHILD process needs separateenv evidence (CI, a container
// or a declared VM), while an in-process test binary needs only the env var.
// A tagged binary on a developer's machine therefore cannot turn the authority
// on, which is what keeps a harness from reaching the owner's live daemon.
func authorityEnabled() bool {
	if !testing.Testing() && separateenv.Current() == "" {
		return false
	}
	return envTrue("PLUMB_ACCOUNT_AUTHORITY")
}

// seamRoot is where the account resolution is redirected to, when anything
// redirects it.
func seamRoot() string {
	if !testing.Testing() && separateenv.Current() == "" {
		return ""
	}
	return ParseTestRoot(os.Getenv("PLUMB_TEST_ACCOUNT_ROOT"))
}
