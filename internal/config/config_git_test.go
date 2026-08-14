package config

import (
	"maps"
	"strings"
	"testing"
	"time"
)

// TestGitWriteTimeout_DefaultIsTenMinutes pins the value, not merely its
// existence. The bound it replaces was two minutes under a comment calling that
// generous for a slow pre-commit hook — the claim this whole change exists to
// correct — so a default that silently drifted back down would restore the
// defect while every other test still passed.
func TestGitWriteTimeout_DefaultIsTenMinutes(t *testing.T) {
	if got := Defaults().Git.WriteTimeout.Duration; got != 10*time.Minute {
		t.Errorf("default git.write_timeout = %s, want 10m", got)
	}
}

// TestGitWriteTimeout_ProjectCannotSetItUntrusted is the trust boundary. The
// knob is a safety decision in both directions: a large value lets a wedged
// child pin the per-repository lock against every other session on the machine,
// and a small one makes plumb SIGKILL git mid-commit and strand an index.lock.
// A cloned repository must not be able to choose either without `plumb trust`,
// which it gets for free by living inside [git] — provided it really is inside
// the block forceCapabilityFieldsToBase resets whole.
func TestGitWriteTimeout_ProjectCannotSetItUntrusted(t *testing.T) {
	ws := t.TempDir()
	writeProjectConfig(t, ws, "[git]\nwrite_timeout = \"1ms\"\n")

	got, err := LoadProject(Defaults(), ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if got.Git.WriteTimeout.Duration != 10*time.Minute {
		t.Errorf("an untrusted project set git.write_timeout to %s; it must stay at the global value", got.Git.WriteTimeout.Duration)
	}

	// And the request must be VISIBLE rather than silently dropped, or the user
	// has nothing to approve and no way to see what was ignored.
	st, err := ProjectPolicyStatusFor(ws)
	if err != nil {
		t.Fatalf("ProjectPolicyStatusFor: %v", err)
	}
	if !st.Asked("git.write_timeout") {
		t.Errorf("the trust disclosure does not mention the request: %v", st.Spec.Keys())
	}
}

// TestGitWriteTimeout_TrustedProjectAndEnvOverride covers the two layers that
// SHOULD be able to move it: a trusted project file, and the environment (which
// is applied after the untrusted fields are forced back, so it must survive).
func TestGitWriteTimeout_TrustedProjectAndEnvOverride(t *testing.T) {
	ws := t.TempDir()
	writeProjectConfig(t, ws, "[git]\nwrite_timeout = \"25m\"\n")
	store := tempTrustStore(t)
	trustWorkspace(t, store, ws)

	got, err := LoadProject(Defaults(), ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if got.Git.WriteTimeout.Duration != 25*time.Minute {
		t.Errorf("trusted project git.write_timeout = %s, want 25m", got.Git.WriteTimeout.Duration)
	}

	t.Setenv("PLUMB_GIT_WRITE_TIMEOUT", "90s")
	got, err = LoadProject(Defaults(), ws)
	if err != nil {
		t.Fatalf("LoadProject with env: %v", err)
	}
	if got.Git.WriteTimeout.Duration != 90*time.Second {
		t.Errorf("env git.write_timeout = %s, want 90s — env is the highest-priority layer", got.Git.WriteTimeout.Duration)
	}
}

// TestGitWriteTimeout_NegativeRejected_ZeroAccepted pins the asymmetry. A
// negative duration is nonsense and is refused at load; ZERO is accepted and
// resolves to the compiled default at the point of use, because the one value
// that must be unreachable is "no bound at all" — an unbounded git child holds
// the repository lock and the shutdown drain for as long as it lives.
func TestGitWriteTimeout_NegativeRejected_ZeroAccepted(t *testing.T) {
	cfg := Defaults()
	cfg.Git.WriteTimeout = Duration{-time.Second}
	err := validate(cfg)
	if err == nil {
		t.Fatal("a negative git.write_timeout must be refused at load")
	}
	if !strings.Contains(err.Error(), "git.write_timeout") {
		t.Errorf("the error must name the offending key, got %v", err)
	}

	cfg.Git.WriteTimeout = Duration{0}
	if err := validate(cfg); err != nil {
		t.Errorf("zero must load (it means the compiled default), got %v", err)
	}
}

// TestLoadProject_GitEnvComposesIdenticallyForEverySpelling pins the composition
// rule for a TRUSTED project's [git] env: the project's value wins for the keys
// it names, and a global entry it is silent about survives — for all three TOML
// spellings of the same intent.
//
// Without composeGitEnv the spelling decides the answer, because go-toml treats
// an inline `env = {…}` as a replacement of the pre-populated map while the
// sub-table and dotted forms merge into it. Two ways of writing the same thing
// would then hand the git child two different environments, and the inline one
// would silently drop the user's global entry.
func TestLoadProject_GitEnvComposesIdenticallyForEverySpelling(t *testing.T) {
	for _, tc := range []struct{ name, payload string }{
		{"inline table", "[git]\nenv = { PROJ = \"2\" }\n"},
		{"sub-table", "[git.env]\nPROJ = \"2\"\n"},
		{"dotted key", "git.env.PROJ = \"2\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			writeProjectConfig(t, ws, tc.payload)
			store := tempTrustStore(t)
			trustWorkspace(t, store, ws)

			base := Defaults()
			base.Git.Env = map[string]string{"GLOBAL": "1"}

			got, err := LoadProject(base, ws)
			if err != nil {
				t.Fatalf("LoadProject: %v", err)
			}
			want := map[string]string{"GLOBAL": "1", "PROJ": "2"}
			if !maps.Equal(got.Git.Env, want) {
				t.Errorf("resolved git env = %v, want %v — every spelling must compose the same way", got.Git.Env, want)
			}
		})
	}
}

// TestLoadProject_GitEnvProjectValueWinsOverGlobal pins the other half of the
// rule: composing must not resurrect a global value the project deliberately
// overrode. A rule that let the global win would make the knob unusable for its
// motivating case (a repository that needs GOWORK=off despite the user's
// global default).
func TestLoadProject_GitEnvProjectValueWinsOverGlobal(t *testing.T) {
	ws := t.TempDir()
	writeProjectConfig(t, ws, "[git.env]\nGOWORK = \"off\"\n")
	store := tempTrustStore(t)
	trustWorkspace(t, store, ws)

	base := Defaults()
	base.Git.Env = map[string]string{"GOWORK": "auto", "KEEP": "yes"}

	got, err := LoadProject(base, ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	want := map[string]string{"GOWORK": "off", "KEEP": "yes"}
	if !maps.Equal(got.Git.Env, want) {
		t.Errorf("resolved git env = %v, want %v", got.Git.Env, want)
	}
	// Composing writes into merged's map; base must be untouched by it.
	if base.Git.Env["GOWORK"] != "auto" {
		t.Errorf("composing wrote back into the caller's base config: %v", base.Git.Env)
	}
}
