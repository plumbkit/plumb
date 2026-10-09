package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tasks_maketarget_test.go covers the Makefile half of PLAN-494's remedy: an
// unconfigured slot whose verb the project's own Makefile defines is told about
// that target, so the caller lands on `make vuln` instead of raw shell.
//
// It lives in this package because the remedy is looked up in the directory the
// command would RUN IN — which only this layer knows (review round 1, S1).

func TestMakefileHasTarget(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{"a plain target", "vuln:\n\tgovulncheck ./...\n", true},
		{"a target with prerequisites", "vuln: build\n\tgovulncheck ./...\n", true},
		{"a target with a comment", "vuln: ## run govulncheck\n", true},
		{"an assignment is not a target", "vuln := govulncheck\n", false},
		{"a tab-indented recipe line", "all:\n\tvuln:\n", false},
		{"a comment", "# vuln: does the thing\n", false},
		{"a target that merely contains the name", "vulnerable:\n\tcheck\n", false},
		{"the name as a prerequisite only", "all: vuln\n", false},
		{"a multi-target line plumb does not guess at", "vuln lint:\n\tcheck\n", false},
		{"carriage returns", "vuln:\r\n\tcheck\r\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := makefileHasTarget(tc.src, "vuln"); got != tc.want {
				t.Errorf("makefileHasTarget(%q) = %v, want %v", tc.src, got, tc.want)
			}
		})
	}
}

func TestMakeTargetRemedy(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Makefile"),
		[]byte("build:\n\tgo build ./...\n\nvuln:\n\tgovulncheck ./...\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := makeTargetRemedy(dir, "vuln")
	for _, want := range []string{"Makefile", `"vuln" target`, "make vuln", "plumb trust"} {
		if !strings.Contains(got, want) {
			t.Errorf("remedy is missing %q; got: %q", want, got)
		}
	}
	// A slot the Makefile does not define gets no invented advice.
	if got := makeTargetRemedy(dir, "lint"); got != "" {
		t.Errorf("makeTargetRemedy for an undefined target = %q, want empty", got)
	}
	// Nor does a directory with no Makefile, or no directory at all.
	if got := makeTargetRemedy(t.TempDir(), "vuln"); got != "" {
		t.Errorf("makeTargetRemedy without a Makefile = %q, want empty", got)
	}
	if got := makeTargetRemedy("", "vuln"); got != "" {
		t.Errorf("makeTargetRemedy without a directory = %q, want empty", got)
	}
}

// TestMakeTargetRemedy_PrefersTheFileMakeWouldRead pins the precedence: GNU make
// reads GNUmakefile before makefile before Makefile, so the remedy must name the
// file whose target would actually run.
func TestMakeTargetRemedy_PrefersTheFileMakeWouldRead(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte("vuln:\n\tthe-one-make-would-not-read\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "GNUmakefile"), []byte("vuln:\n\tthe-one-make-reads\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := makeTargetRemedy(dir, "vuln"); !strings.Contains(got, "GNUmakefile") {
		t.Errorf("remedy = %q, want it to name GNUmakefile", got)
	}
}

// TestRunTask_RemedyComesFromTheDestination is S1's regression. The verb's target
// lives in a SUBDIRECTORY's Makefile — plumb-ops's own shape, where `vuln` is in
// plumb/Makefile and the workspace root has none — so a remedy that only reads
// the workspace root finds nothing and the caller is sent to raw shell.
func TestRunTask_RemedyComesFromTheDestination(t *testing.T) {
	ws := t.TempDir()
	module := filepath.Join(ws, "plumb")
	if err := os.MkdirAll(module, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(module, "Makefile"), []byte("vuln:\n\tgovulncheck ./...\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The workspace root has a Makefile WITHOUT the target: the old version read
	// this one, found no `vuln`, and said nothing about the project's gate.
	if err := os.WriteFile(filepath.Join(ws, "Makefile"), []byte("build:\n\tgo build ./...\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewTasks(WriteDeps{Boundary: testBoundaryGuard(ws)}, func(_ context.Context, _ TaskRequest) (TaskCommand, error) {
		// The resolver's answer for a slot with no command: context only.
		return TaskCommand{Slot: "vuln", Language: "go", Configured: []string{"build"}, Root: ws}, nil
	})

	// (a) no path: the remedy names what the run directory has, and here that is
	// nothing, so the refusal must NOT invent a target.
	if _, err := callRunTask(t, tool, map[string]any{"slot": "vuln"}); err == nil ||
		strings.Contains(err.Error(), "make vuln") {
		t.Errorf("without a destination the remedy must not claim a root target that does not exist: %v", err)
	}

	// (b) with the destination named, the same refusal names the project's gate.
	_, err := callRunTask(t, tool, map[string]any{"slot": "vuln", "path": module})
	if err == nil {
		t.Fatal("an unconfigured slot must be refused")
	}
	for _, want := range []string{"make vuln", "Makefile"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal is missing %q — the remedy must come from the destination:\n%v", want, err)
		}
	}
}
