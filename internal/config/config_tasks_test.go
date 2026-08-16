package config

import "testing"

func TestDefaults_TasksPresentForGo(t *testing.T) {
	got := Defaults().Tasks["go"]
	if got.Build != "go build ./..." {
		t.Errorf("go build default missing: %+v", got)
	}
	// The test slot carries a DEFAULTED placeholder rather than a bare `./...`.
	// That is the whole of PLAN-326: with no placeholder, run_task's target and
	// mutation_test's test_target are refused on an unmodified install, so a
	// mutation run costs the entire suite per mutant. The default inside it is
	// what keeps a bare `run_task {slot: "test"}` running everything, unchanged.
	if got.Test != "go test {target:./...}" {
		t.Errorf("go test default must keep a defaulted {target} placeholder, got %q", got.Test)
	}
	if got.Verify != "" {
		t.Errorf("verify slot should be empty (composite), got %q", got.Verify)
	}
}

// TestDefaults_EveryTestDefaultIsScopable states the property the literal above
// is only one instance of: if a shipped test command cannot be scoped, the
// scoping features do not exist for that language's users.
//
// It deliberately allows a language to opt OUT by having no test command at all,
// but not to ship one that silently refuses a target.
func TestDefaults_EveryTestDefaultIsScopable(t *testing.T) {
	// typescript, swift and zig scope through runner-specific FLAGS, not a
	// positional argument; a guessed flag that is wrong is worse than none, so
	// they are listed here as a decision rather than an oversight.
	flagScoped := map[string]bool{"typescript": true, "swift": true, "zig": true}

	for lang, tc := range Defaults().Tasks {
		if tc.Test == "" || flagScoped[lang] {
			continue
		}
		argv, err := ParseTaskCommand(tc.Test)
		if err != nil {
			t.Errorf("tasks.%s.test does not parse: %v", lang, err)
			continue
		}
		if countTargetTokens(argv) != 1 {
			t.Errorf("tasks.%s.test = %q has no {target} placeholder, so run_task target and "+
				"mutation_test test_target are an ERROR for %s users on a default install", lang, tc.Test, lang)
		}
	}
}

func TestClone_TasksDeepCopied(t *testing.T) {
	base := Defaults()
	cl := cloneConfig(base)
	cl.Tasks["go"] = TasksConfig{Build: "mutated"}
	if base.Tasks["go"].Build == "mutated" {
		t.Error("cloneConfig did not deep-copy the Tasks map")
	}
}

func TestParseTaskCommand(t *testing.T) {
	cases := []struct {
		in      string
		wantLen int
		wantErr bool
	}{
		{"go build ./...", 3, false},
		{"  go   test  ./...  ", 3, false},
		{"", 0, false}, // empty slot is valid, no argv
		{"go test {target}", 3, false},
		{"go build ./... && go test ./...", 0, true},
		{"rm -rf / ; echo hi", 0, true},
		{"cat foo | grep bar", 0, true},
		{"echo $(whoami)", 0, true},
		{"echo `id`", 0, true},
		{"go test > out.txt", 0, true},
	}
	for _, c := range cases {
		argv, err := ParseTaskCommand(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseTaskCommand(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && len(argv) != c.wantLen {
			t.Errorf("ParseTaskCommand(%q) len=%d, want %d", c.in, len(argv), c.wantLen)
		}
	}
}

func TestValidateTasks_RejectsShellMeta(t *testing.T) {
	tasks := map[string]TasksConfig{"go": {Build: "go build ./... && rm -rf /"}}
	if err := validateTasks(tasks); err == nil {
		t.Error("validateTasks accepted a command with &&")
	}
	if err := validateTasks(Defaults().Tasks); err != nil {
		t.Errorf("validateTasks rejected the shipped defaults: %v", err)
	}
}

// TestValidateTasks_ChecksVerifySlot guards the verify slot: it is agent-writable
// yet was previously skipped by validateTasks (Get("verify") returns ""), so a
// metacharacter command could be staged unchecked. Reading the field directly
// must now reject it.
func TestValidateTasks_ChecksVerifySlot(t *testing.T) {
	tasks := map[string]TasksConfig{"go": {Verify: "go test ./... ; rm -rf /"}}
	if err := validateTasks(tasks); err == nil {
		t.Error("validateTasks accepted a verify command with a shell metacharacter")
	}
}
