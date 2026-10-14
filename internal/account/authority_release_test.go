//go:build !plumbtest

package account

import "testing"

// The RELEASED-build answers, which the seam's constant would otherwise make
// unreachable from a test binary. They are UNTAGGED-only on purpose: the tagged
// build is a harness with a different rule (see authority_tagged_test.go), and a
// single shared file asserting one variant's values is what let the tagged tree
// compile against helpers that did not exist there.
//
// Both are driven through the ...For functions so a mutation to either branch is
// caught by NAME rather than only by the container probe.

func TestAuthorityEnabled_ReleasedBuildsUseTheResolvedConfiguration(t *testing.T) {
	freshAuthority(t)
	t.Setenv(AuthorityEnvVar, "")
	if !authorityEnabledFor(false) {
		t.Fatal("a released build with nothing set must run the account authority: that is the flip")
	}
	// And the configuration can still turn it off, which is the point of the key.
	SetResolvedAuthority(false)
	if authorityEnabledFor(false) {
		t.Error("a released build ignored the resolved opt-out")
	}
}

func TestConfigAuthorityEnabled_ReleasedBuildsNeverMoveTheConfigFile(t *testing.T) {
	freshAuthority(t)
	// An EXPLICIT environment opt-out, the strongest form: even then the config
	// file must stay the account's, or the file that records the opt-out would
	// move out from under itself and `plumb config set` would write a file the
	// next start never reads.
	t.Setenv(AuthorityEnvVar, "false")
	if !configAuthorityEnabledFor(false) {
		t.Error("a released build moved the config directory with the authority switch")
	}
	if authorityEnabledFor(false) {
		t.Error("the environment opt-out did not turn the authority off in a released build")
	}
}
