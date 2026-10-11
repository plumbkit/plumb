package config

import (
	"fmt"
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

// TestEnvKeySteersExecution pins the taxonomy both consumers share: the
// disclosure warning `plumb trust` prints, and the per-call run_task gate. A name
// listed here a project may still set (trust discloses it); a call may not touch
// it at all.
func TestEnvKeySteersExecution(t *testing.T) {
	// The last eight were added for #634's per-call gate: each one decides where a
	// build's code or its tooling comes from. GOWORK can name a go.work outside the
	// workspace entirely, GOMODCACHE/GOPATH move the module source, and make reads
	// its flags and its makefiles out of the environment.
	for _, k := range []string{
		"PATH", "GOFLAGS", "GIT_SSH_COMMAND", "NODE_OPTIONS", "PYTHONPATH", "RUSTC_WRAPPER", "LD_LIBRARY_PATH", "DYLD_LIBRARY_PATH",
		"GOWORK", "GOMODCACHE", "GOPATH", "MAKEFLAGS", "MAKEFILES", "PYTEST_ADDOPTS", "RUSTUP_TOOLCHAIN", "_JAVA_OPTIONS",
	} {
		if !EnvKeySteersExecution(k) {
			t.Errorf("EnvKeySteersExecution(%q) = false, want the trust disclosure to flag it", k)
		}
	}
	for _, k := range []string{"GOTMPDIR", "TMPDIR", "CI", "RUST_BACKTRACE", "PLUMB_MODE"} {
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

// TestTaskEnvKeyOf recognises only the env table's spelling, in any case, so a
// trust-hash entry is never mistaken for, or confused with, a command slot.
func TestTaskEnvKeyOf(t *testing.T) {
	for slot, want := range map[string]string{"env.GOTMPDIR": "GOTMPDIR", "ENV.X": "X"} {
		if got, ok := TaskEnvKeyOf(slot); !ok || got != want {
			t.Errorf("TaskEnvKeyOf(%q) = %q, %v; want %q", slot, got, ok, want)
		}
	}
	for _, slot := range []string{"test", "working_dir", "check.x", "envx.Y"} {
		if _, ok := TaskEnvKeyOf(slot); ok {
			t.Errorf("TaskEnvKeyOf(%q) reported an env entry", slot)
		}
	}
}

// TestValidateTaskEnvOverrides pins what ONE run_task call may do to the child's
// environment (#634). Both rules are guard rails rather than a boundary — the
// name list cannot be completed, since make reads any variable — so what this
// pins is the direction of each refusal and the fact that neither list is exempt.
func TestValidateTaskEnvOverrides(t *testing.T) {
	for _, tc := range []struct {
		name  string
		set   map[string]string
		unset []string
	}{
		{"a mode switch and a corpus path", map[string]string{"MODE": "experiment", "CORPUS": "/tmp/corpus"}, nil},
		{"punctuation in a value", map[string]string{"FILTER": "a or b; not c", "URL": "https://x.example/y?z=1"}, nil},
		{"the config placeholders", map[string]string{"GOTMPDIR": "{workspace}/.testcache"}, nil},
		{"an empty value", map[string]string{"CI": ""}, nil},
		{"removing inert names", nil, []string{"GOTMPDIR", "TMPDIR", "CI"}},
		{"nothing at all", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateTaskEnvOverrides(tc.set, tc.unset); err != nil {
				t.Errorf("refused: %v", err)
			}
		})
	}

	for _, tc := range []struct {
		name    string
		set     map[string]string
		unset   []string
		wantErr string
	}{
		{"loader injection", map[string]string{"LD_PRELOAD": "/tmp/x.so"}, nil, "dynamic-loader"},
		{"the program the command runs", map[string]string{"PATH": "/tmp/bin"}, nil, "changes which program"},
		{"go's own flags", map[string]string{"GOFLAGS": "-toolexec=/tmp/x"}, nil, "changes which program"},
		{"a command git runs", map[string]string{"GIT_SSH_COMMAND": "/tmp/x"}, nil, "changes which program"},
		{"a steering family by prefix", map[string]string{"DYLD_PRINT_LIBRARIES": "1"}, nil, "changes which program"},
		{"a name that is not portable", map[string]string{"bad name": "x"}, nil, "not a valid environment variable name"},
		{"a newline in the value", map[string]string{"MODE": "a\nb"}, nil, "control characters"},
		{"a NUL in the value", map[string]string{"MODE": "a\x00b"}, nil, "NUL"},
		{"an over-long value", map[string]string{"MODE": strings.Repeat("x", maxTaskEnvOverride+1)}, nil, "longer than"},
		{"the same name both ways", map[string]string{"GOTMPDIR": "/tmp"}, []string{"GOTMPDIR"}, "both env and unset"},
		{"an unset name that is not portable", nil, []string{"bad name"}, "not a valid environment variable name"},
		// The same name rule applies to REMOVAL (#634 review): taking a
		// configured GOFLAGS=-mod=vendor or GOWORK=off back out changes what the
		// command builds as surely as putting one in, and the state being removed
		// is the one `plumb trust` approved.
		{"removing a configured steering name", nil, []string{"GOFLAGS"}, "changes what the stored command would run"},
		{"removing a name that decides the module tree", nil, []string{"GOWORK"}, "changes what the stored command would run"},
		{"removing a loader variable", nil, []string{"LD_PRELOAD"}, "dynamic-loader"},
		// A value goes through the same placeholder rule as a config entry, so a
		// typo is refused here rather than reaching the command as literal text.
		{"an unknown placeholder in a value", map[string]string{"GOTMPDIR": "{workpsace}/x"}, nil, "unknown placeholder"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTaskEnvOverrides(tc.set, tc.unset)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("= %v, want a refusal containing %q", err, tc.wantErr)
			}
		})
	}

	// The cap counts both directions together.
	set := make(map[string]string, maxTaskEnvOverrides)
	for i := range maxTaskEnvOverrides {
		set[fmt.Sprintf("VAR_%d", i)] = "1"
	}
	if err := ValidateTaskEnvOverrides(set, nil); err != nil {
		t.Errorf("exactly the cap must be accepted: %v", err)
	}
	set["ONE_MORE"] = "1"
	if err := ValidateTaskEnvOverrides(set, nil); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("one over the cap = %v, want a refusal naming the limit", err)
	}
	if err := ValidateTaskEnvOverrides(nil, make([]string, maxTaskEnvOverrides+1)); err == nil {
		t.Error("an unset list over the cap must be refused too")
	}
}

// TestValidateTaskEnvOverrides_RemovalSharesTheNameRule: `unset` is held to the
// same name rule as `env`, which was the other way round in the first draft —
// where it was justified with "a removal can only shrink the environment". That
// is false for a CONFIGURED entry: removing `GOFLAGS=-mod=vendor` changes what
// compiles, and removing `GOPROXY=off` turns a hermetic build into a live fetch.
// The names that matter for the motivating case (GOTMPDIR, TMPDIR) are not
// steering names, so nothing it was written for is lost.
func TestValidateTaskEnvOverrides_RemovalSharesTheNameRule(t *testing.T) {
	if err := ValidateTaskEnvOverrides(nil, []string{"GOTMPDIR", "TMPDIR"}); err != nil {
		t.Errorf("removing a scratch-directory name must stay allowed: %v", err)
	}
	for name, want := range map[string]string{
		"GOFLAGS":    "changes what the stored command would run",
		"PATH":       "changes what the stored command would run",
		"LD_PRELOAD": "dynamic-loader",
	} {
		err := ValidateTaskEnvOverrides(nil, []string{name})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("unset %s = %v, want a refusal containing %q", name, err, want)
		}
	}
}

// TestValidateTaskEnvOverrides_RefusalIsDeterministic: a call that is wrong in
// several ways is refused for the SAME reason every time. Go's map iteration is
// randomised, so without the sorted walk a retrying caller reads a different
// problem on each attempt and cannot tell which one it just fixed.
func TestValidateTaskEnvOverrides_RefusalIsDeterministic(t *testing.T) {
	set := map[string]string{"PATH": "/x", "LD_PRELOAD": "/y", "bad name": "z"}
	first := ""
	for range 20 {
		err := ValidateTaskEnvOverrides(set, nil)
		if err == nil {
			t.Fatal("must be refused")
		}
		if first == "" {
			first = err.Error()
			continue
		}
		if err.Error() != first {
			t.Fatalf("the refusal changed between calls:\n%v\nthen\n%v", first, err)
		}
	}
	// Sorted, the loader name comes first, so THAT is the reason reported.
	if !strings.Contains(first, "LD_PRELOAD") {
		t.Errorf("= %q, want the alphabetically-first offender", first)
	}

	// The unset list is walked too, and refusal order across the two lists is
	// fixed: a bad unset name is reported before a bad set name of the same call.
	mixed := ValidateTaskEnvOverrides(map[string]string{"PATH": "/x"}, []string{"LD_PRELOAD"})
	if mixed == nil || !strings.Contains(mixed.Error(), "unset LD_PRELOAD") {
		t.Errorf("= %v, want the unset refusal to be deterministic and first", mixed)
	}
}
