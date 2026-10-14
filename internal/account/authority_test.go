package account

import "testing"

// The account-authority gate, after the PLAN-518 default flip.
//
// These tests drive the value machine directly, because that is the only place
// the flip's decision is made: internal/paths reads it through AuthorityEnabled,
// internal/config RECORDS it from `daemon.account_authority`, and a released
// build is the only process in which those two are the same reading.

// freshAuthority clears the recorded value for one test, so a case can drive the
// seed and the resolution independently, and leaves the package as it found it.
func freshAuthority(t *testing.T) {
	t.Helper()
	resetAuthorityForTest()
	t.Cleanup(resetAuthorityForTest)
}

func TestSeedAuthority_DefaultsOnAndFollowsTheEnvironment(t *testing.T) {
	for _, tc := range []struct {
		env   string
		unset bool
		want  bool
	}{
		{unset: true, want: true}, // the flip: nothing set means ON
		{env: "", want: true},     // an empty value is the same as unset
		{env: "1", want: true},
		{env: "true", want: true},
		{env: "yes", want: true},
		{env: "on", want: true},
		// Anything the parser does not recognise turns the authority ON, which is
		// the safe reading for a default-true switch: an unanticipated value must
		// not be the thing that quietly moves the account's daemon into $HOME.
		{env: "banana", want: true},
		{env: "0", want: false},
		{env: "false", want: false},
		{env: "no", want: false},
	} {
		t.Run("env="+tc.env, func(t *testing.T) {
			freshAuthority(t)
			if tc.unset {
				t.Setenv(AuthorityEnvVar, "")
				// t.Setenv cannot unset, and ParseAuthorityEnv treats empty as
				// unset — which is the same reading, and the one the daemon sees
				// from a launcher that passes nothing.
			} else {
				t.Setenv(AuthorityEnvVar, tc.env)
			}
			if got := seedAuthorityValue(); got != tc.want {
				t.Errorf("seedAuthorityValue() = %v, want %v", got, tc.want)
			}
		})
	}
}

// The flip itself: a process that has resolved nothing runs with the account
// authority ON, which is the whole of rollout invariant 1's final step.
func TestResolvedAuthority_DefaultsOn(t *testing.T) {
	freshAuthority(t)
	t.Setenv(AuthorityEnvVar, "")
	if !resolvedAuthorityValue() {
		t.Fatal("an unresolved process must default to the account authority: the compiled default is ON")
	}
	if !AuthorityResolved() {
		t.Fatal("AuthorityResolved must report the same compiled default")
	}
}

// A read BEFORE the first resolution must not decide. The daemon resolves the
// runtime directory its lifetime lock lives in before it can read any
// configuration (it takes its locks first, by design), so a seed that latched on
// that read would ignore a `daemon.account_authority = false` the user had
// written down — and the daemon would then look for its lock somewhere its own
// launcher did not put one.
func TestResolvedAuthority_AnEarlyReadDoesNotLatchTheSeed(t *testing.T) {
	freshAuthority(t)
	t.Setenv(AuthorityEnvVar, "")
	if !resolvedAuthorityValue() {
		t.Fatal("precondition: the seed defaults to ON")
	}
	if authorityRecordedForTest() {
		t.Fatal("reading the gate recorded a resolution; only internal/config may resolve it")
	}
	// Now the configuration is read and says OFF.
	SetResolvedAuthority(false)
	if got := resolvedAuthorityValue(); got {
		t.Error("resolvedAuthorityValue() = true after the configuration resolved false: an early read latched the seed")
	}
}

func TestResolvedAuthority_TheFirstResolutionWins(t *testing.T) {
	freshAuthority(t)
	SetResolvedAuthority(false)
	SetResolvedAuthority(true) // a later reload of the same process
	if got := resolvedAuthorityValue(); got {
		t.Error("a second SetResolvedAuthority changed the value: the gate is read once, and a change needs a restart")
	}
	if !authorityRecordedForTest() {
		t.Error("recording the value did not mark it resolved")
	}
}

func TestAuthorityResolved_ReportsTheConfigurationNotTheSeam(t *testing.T) {
	freshAuthority(t)
	t.Setenv(AuthorityEnvVar, "")
	SetResolvedAuthority(false)
	// In a test binary AuthorityEnabled follows the seam (the environment), so it
	// stays false here too — but the two questions must not be the same function:
	// AuthorityResolved is what internal/config's tests assert the wiring through,
	// and a released build's AuthorityEnabled is exactly this value.
	if AuthorityResolved() {
		t.Error("AuthorityResolved() = true after the configuration resolved false")
	}
}

// The seam's own rule, and the reason the flip cannot promote the existing
// suite: in a test binary the gate is the environment, so every test written
// against the environment-derived resolution keeps it. D5 requires exactly this.
func TestAuthorityEnabled_InATestBinaryTheSeamDecides(t *testing.T) {
	freshAuthority(t)
	t.Setenv(AuthorityEnvVar, "")
	if AuthorityEnabled() {
		t.Fatal("a test binary with nothing set must resolve the legacy way")
	}
	if ConfigAuthorityEnabled() {
		t.Fatal("a test binary must keep the environment's config directory, or a test would read the developer's real one")
	}
	t.Setenv(AuthorityEnvVar, "1")
	if !AuthorityEnabled() {
		t.Fatal("the environment asked for the account authority and did not get it")
	}
	if !ConfigAuthorityEnabled() {
		t.Fatal("the environment asked for the account authority and the config directory did not follow")
	}
	// And the resolved value cannot override the seam in either direction: a
	// configuration that resolved ON must not promote the suite either.
	SetResolvedAuthority(true)
	t.Setenv(AuthorityEnvVar, "")
	if AuthorityEnabled() {
		t.Error("a resolved value overrode the seam in a test binary; the seam is the gate there")
	}
}

// The RELEASED-build answers, which the seam's constant would otherwise make
// unreachable from a test binary. Both are driven through the ...For functions so
// a mutation to either branch is caught by NAME rather than only by the container
// probe.
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
