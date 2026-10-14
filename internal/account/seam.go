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
func authorityEnabled() bool { return authorityEnabledFor(testing.Testing()) }

// authorityEnabledFor is authorityEnabled with the build fact that selects the
// in-process rule passed in. It exists so this file and seam_off.go present the
// SAME shape: a test shared by both variants compiles against either, and the
// tagged rule below is reachable by a test rather than only by a container suite.
func authorityEnabledFor(inTestBinary bool) bool {
	return taggedAuthorityFor(inTestBinary, separateenv.Current() != "")
}

// taggedAuthorityFor is the tagged rule with BOTH of its inputs passed in: is
// this process the test binary, and is there separateenv evidence (CI, a
// container marker, a declared VM)?
//
// The evidence is an argument rather than a direct call so the rule can be
// asserted in every direction from any environment. A container always has
// evidence and a developer's laptop never does, so a test that read
// separateenv.Current() itself would silently stop covering one half of the rule
// depending on where it ran — the same "compiles but never runs" trap that left
// this file's helpers missing while only the untagged variant was ever built.
func taggedAuthorityFor(inTestBinary, haveSeparateEnvEvidence bool) bool {
	if !inTestBinary && !haveSeparateEnvEvidence {
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
func configAuthorityEnabled() bool { return configAuthorityEnabledFor(testing.Testing()) }

// configAuthorityEnabledFor is configAuthorityEnabled with the build fact passed
// in. In the tagged build the config-directory answer IS the authority answer,
// and the tagged tests pin that agreement rather than restating the rule.
func configAuthorityEnabledFor(inTestBinary bool) bool { return authorityEnabledFor(inTestBinary) }
