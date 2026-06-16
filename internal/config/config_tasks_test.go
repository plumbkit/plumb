package config

import "testing"

func TestDefaults_TasksPresentForGo(t *testing.T) {
	got := Defaults().Tasks["go"]
	if got.Build != "go build ./..." || got.Test != "go test ./..." {
		t.Errorf("go task defaults missing: %+v", got)
	}
	if got.Verify != "" {
		t.Errorf("verify slot should be empty (composite), got %q", got.Verify)
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
