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
//
// seamRoot is where the account resolution is redirected to, when anything
// redirects it.
func seamRoot() string {
	if !testing.Testing() && separateenv.Current() == "" {
		return ""
	}
	return ParseTestRoot(os.Getenv("PLUMB_TEST_ACCOUNT_ROOT"))
}

// authorityEnabled reports whether the account authority drives this process.
//
// A TAGGED build — the smoke suites' children — keeps the pre-flip reading:
// PLUMB_ACCOUNT_AUTHORITY, default OFF, and nothing at all without separateenv
// evidence (CI, a container, a declared VM), so a tagged binary on a developer's
// machine still cannot take the authority. The container end-to-end evidence for
// the flip is produced from an UNTAGGED build, which is the build users get; a
// harness that wants the canonical resolution asks for it explicitly, exactly as
// it does today.
func authorityEnabled() bool {
	if !testing.Testing() && separateenv.Current() == "" {
		return false
	}
	return authorityFromEnv()
}

// configAuthorityEnabled is the config-directory half of the gate. A TAGGED
// build is a harness, not a user's plumb: it keeps the environment's config
// directory whenever the harness has not asked for the canonical resolution, so
// cmd/smoke's per-test HOME and XDG still isolate one test from the next. The
// account-directory rule a released build follows (the config file never moves;
// see seam_off.go) does not apply here, because there is no user configuration
// to keep readable — the harness says which directories it wants.
func configAuthorityEnabled() bool { return authorityEnabled() }
