package config

import (
	"maps"
	"testing"
)

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
