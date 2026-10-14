//go:build plumbtest

package account

import (
	"testing"

	"github.com/plumbkit/plumb/internal/separateenv"
)

// The TAGGED seam's own answers.
//
// A tagged build is a harness — cmd/smoke's and cmd/clientsmoke's children, built
// with `-tags=plumbtest` — never a user's plumb, so its rule is deliberately not
// the released build's: the ENVIRONMENT decides, and out of process it decides
// only with separateenv evidence (CI, a container marker, a declared VM), which is
// what keeps a tagged binary on a developer's machine away from the owner's live
// daemon.
//
// These live in their own tagged file because the untagged assertions above are
// about a different rule. That split is the fix for a red gate: the shared file
// used to call helpers that only seam_off.go defined, so anything building the
// tree with `-tags=plumbtest` (make lint-tags, the smoke suites, CI's tagged legs)
// failed to compile — while `go test ./internal/account/` stayed green.

func TestTaggedAuthority_InProcessIsTheEnvironmentAlone(t *testing.T) {
	freshAuthority(t)
	t.Setenv(AuthorityEnvVar, "")
	if authorityEnabledFor(true) {
		t.Fatal("a tagged test binary with nothing set must keep the legacy resolution")
	}
	if configAuthorityEnabledFor(true) {
		t.Fatal("a tagged test binary must keep the environment's config directory")
	}
	t.Setenv(AuthorityEnvVar, "1")
	if !authorityEnabledFor(true) {
		t.Fatal("the harness asked for the account authority and did not get it")
	}
	// A configuration that resolved ON must not promote a harness either: the
	// resolved value belongs to the released build's rule.
	SetResolvedAuthority(true)
	t.Setenv(AuthorityEnvVar, "")
	if authorityEnabledFor(true) {
		t.Error("a resolved value overrode the tagged seam")
	}
}

// The evidence requirement, asserted in BOTH directions through the two-input
// rule, so it holds on a developer's laptop (no evidence) and in a container
// (evidence) alike — and so a mutant that drops it is killed wherever the suite
// runs, not only where the environment happens to cooperate.
func TestTaggedAuthority_OutOfProcessNeedsSeparateEnvEvidence(t *testing.T) {
	freshAuthority(t)
	t.Setenv(AuthorityEnvVar, "1") // the harness explicitly asked for the authority

	if taggedAuthorityFor(false, false) {
		t.Error("a tagged child with NO separateenv evidence took the account authority")
	}
	if !taggedAuthorityFor(false, true) {
		t.Error("a tagged child in CI or a container did not get the authority it asked for")
	}
	if !taggedAuthorityFor(true, false) {
		t.Error("the test binary itself must not need separateenv evidence")
	}
	// And the live predicate feeds the rule the value this process actually has.
	if got, want := authorityEnabledFor(false), separateenv.Current() != ""; got != want {
		t.Errorf("authorityEnabledFor(false) = %v with separateenv evidence %q, want %v", got, separateenv.Current(), want)
	}
}

// The config-directory half of the tagged rule IS the authority half, and the
// agreement is pinned with the environment OFF: that is the case a "return true"
// in the config helper would break, in a container as much as on a laptop.
func TestTaggedConfigAuthority_FollowsTheTaggedAuthority(t *testing.T) {
	freshAuthority(t)
	t.Setenv(AuthorityEnvVar, "")
	for _, inTestBinary := range []bool{true, false} {
		if got, want := configAuthorityEnabledFor(inTestBinary), authorityEnabledFor(inTestBinary); got != want {
			t.Errorf("configAuthorityEnabledFor(%v) = %v, want the tagged authority answer %v", inTestBinary, got, want)
		}
	}
	if configAuthorityEnabledFor(false) {
		t.Error("a tagged harness with no authority kept the ACCOUNT's config directory")
	}
}
