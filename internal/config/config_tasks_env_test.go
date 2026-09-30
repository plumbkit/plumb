package config

import (
	"maps"
	"strings"
	"testing"
)

// TestLoadProject_TaskEnvComposesIdenticallyForEverySpelling pins [tasks.<lang>]
// env to the composition rule [git] env has: the project's value wins for the
// names it sets and a global entry it is silent about survives, whichever of the
// three TOML spellings the project used. go-toml replaces a pre-populated map for
// the inline form and merges into it for the other two, so without the explicit
// composition two spellings of one intent would give the command different
// environments.
func TestLoadProject_TaskEnvComposesIdenticallyForEverySpelling(t *testing.T) {
	for _, tc := range []struct{ name, payload string }{
		{"inline table", "[tasks.go]\nenv = { PROJ = \"2\" }\n"},
		{"sub-table", "[tasks.go.env]\nPROJ = \"2\"\n"},
		{"dotted key", "tasks.go.env.PROJ = \"2\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			writeProjectConfig(t, ws, tc.payload)

			base := Defaults()
			goTasks := base.Tasks["go"]
			goTasks.Env = map[string]string{"GLOBAL": "1"}
			base.Tasks["go"] = goTasks

			got, err := LoadProject(base, ws)
			if err != nil {
				t.Fatalf("LoadProject: %v", err)
			}
			want := map[string]string{"GLOBAL": "1", "PROJ": "2"}
			if !maps.Equal(got.Tasks["go"].Env, want) {
				t.Errorf("resolved tasks.go.env = %v, want %v — every spelling must compose the same way", got.Tasks["go"].Env, want)
			}
			if got.Tasks["go"].Build != "go build ./..." {
				t.Errorf("a project env must not erase the language's commands, build = %q", got.Tasks["go"].Build)
			}
			if !maps.Equal(base.Tasks["go"].Env, map[string]string{"GLOBAL": "1"}) {
				t.Errorf("loading a project wrote into the CALLER's base env: %v", base.Tasks["go"].Env)
			}
		})
	}
}

// TestLoadProject_TaskEnvIsNotAnExtraSlot guards the second decode: every key
// under [tasks.<lang>] that is not a declared field becomes a project-defined
// slot, and `env` must not — it would surface as a runnable slot named "env".
func TestLoadProject_TaskEnvIsNotAnExtraSlot(t *testing.T) {
	ws := t.TempDir()
	writeProjectConfig(t, ws, "[tasks.go]\nenv = { GOTMPDIR = \"{workspace}/.testcache\" }\n")
	got, err := LoadProject(Defaults(), ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if _, ok := got.Tasks["go"].Extra["env"]; ok {
		t.Errorf("env was decoded as a project-defined slot: %v", got.Tasks["go"].Extra)
	}
	if got.Tasks["go"].Env["GOTMPDIR"] != "{workspace}/.testcache" {
		t.Errorf("tasks.go.env = %v, want the GOTMPDIR entry kept verbatim (placeholders expand at run time)", got.Tasks["go"].Env)
	}
}

// TestProjectTaskCommands_EnvIsInTheTrustHash is the security half of task env:
// an environment variable changes what a command runs as surely as the command
// does (LD_PRELOAD, PATH, GOFLAGS=-toolexec=…), so a project's env must be part
// of the content `plumb trust` binds to. Every spelling — including a table name
// in another case, which go-toml binds to the same field — must be enumerated,
// and a changed value must change the hash.
func TestProjectTaskCommands_EnvIsInTheTrustHash(t *testing.T) {
	for _, payload := range []string{
		"[tasks.go]\nenv = { GOFLAGS = \"-toolexec=/tmp/x\" }\n",
		"[tasks.go.env]\nGOFLAGS = \"-toolexec=/tmp/x\"\n",
		"[TASKS.go]\nENV = { GOFLAGS = \"-toolexec=/tmp/x\" }\n",
	} {
		ws := t.TempDir()
		writeProjectConfig(t, ws, payload)
		cmds, err := ProjectTaskCommands(ws)
		if err != nil {
			t.Fatalf("ProjectTaskCommands: %v", err)
		}
		found := false
		for _, c := range cmds {
			if key, ok := TaskEnvKeyOf(c.Slot); ok && key == "GOFLAGS" && c.Command == "-toolexec=/tmp/x" {
				found = true
			}
		}
		if !found {
			t.Errorf("payload %q: the env entry is missing from the trusted set %+v — it would run with no `plumb trust`", payload, cmds)
		}
	}

	ws := t.TempDir()
	writeProjectConfig(t, ws, "[tasks.go]\nenv = { X = \"1\" }\n")
	before, _ := ProjectTaskCommands(ws)
	writeProjectConfig(t, ws, "[tasks.go]\nenv = { X = \"2\" }\n")
	after, _ := ProjectTaskCommands(ws)
	if canonicalTaskHash(before) == canonicalTaskHash(after) {
		t.Error("changing an env value left the trust hash unchanged — a trusted project could swap it silently")
	}
}

func TestValidateTaskEnv(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{"placeholders", map[string]string{"GOTMPDIR": "{workspace}/.testcache", "OUT": "{working_dir}/out"}, ""},
		{"plain", map[string]string{"GOWORK": "off", "_X1": ""}, ""},
		{"empty name", map[string]string{"": "x"}, "not a valid environment variable name"},
		{"dash in name", map[string]string{"A-B": "x"}, "not a valid environment variable name"},
		{"leading digit", map[string]string{"1X": "x"}, "not a valid environment variable name"},
		{"NUL in value", map[string]string{"X": "a\x00b"}, "NUL"},
		{"typo placeholder", map[string]string{"X": "{workpsace}/x"}, "unknown placeholder"},
		{"LD_PRELOAD", map[string]string{"LD_PRELOAD": "/tmp/x.so"}, "refused"},
		{"LD_AUDIT", map[string]string{"LD_AUDIT": "/tmp/x.so"}, "refused"},
		{"DYLD_INSERT_LIBRARIES", map[string]string{"DYLD_INSERT_LIBRARIES": "/tmp/x.dylib"}, "refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTasks(map[string]TasksConfig{"go": {Env: tc.env}})
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("validateTasks rejected %v: %v", tc.env, err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("validateTasks(%v) = %v, want an error containing %q", tc.env, err, tc.wantErr)
			}
		})
	}
}

// TestEnvKeySteersExecution pins the disclosure warning `plumb trust` prints: the
// keys that change WHICH program or code a command runs are flagged, ordinary
// ones are not.
func TestEnvKeySteersExecution(t *testing.T) {
	for _, k := range []string{"PATH", "GOFLAGS", "GIT_SSH_COMMAND", "NODE_OPTIONS", "PYTHONPATH", "RUSTC_WRAPPER", "LD_LIBRARY_PATH", "DYLD_LIBRARY_PATH"} {
		if !EnvKeySteersExecution(k) {
			t.Errorf("EnvKeySteersExecution(%q) = false, want the trust disclosure to flag it", k)
		}
	}
	for _, k := range []string{"GOTMPDIR", "GOWORK", "CI", "RUST_BACKTRACE"} {
		if EnvKeySteersExecution(k) {
			t.Errorf("EnvKeySteersExecution(%q) = true, want an ordinary variable left unflagged", k)
		}
	}
}

// TestDefaults_TestCommandsCarryRunAndVerbose pins #538's shipped placeholders:
// the go and python test defaults take a test-name filter and a verbose flag, and
// both collapse to nothing when not asked for, so a bare run is unchanged.
func TestDefaults_TestCommandsCarryRunAndVerbose(t *testing.T) {
	for lang, want := range map[string]string{
		"go":     "go test {verbose:-v} {run:-run} {target:./...}",
		"python": "pytest {verbose:-v} {run:-k} {target:}",
		"rust":   "cargo test {target:} {run:--}",
	} {
		if got := Defaults().Tasks[lang].Test; got != want {
			t.Errorf("tasks.%s.test default = %q, want %q", lang, got, want)
		}
	}
}
