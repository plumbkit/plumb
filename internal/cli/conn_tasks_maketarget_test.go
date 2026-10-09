package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// conn_tasks_maketarget_test.go covers the Makefile half of PLAN-494's remedy:
// an unconfigured slot whose verb the workspace's own Makefile defines is told
// about that target, so the caller lands on the project's gate (`make vuln`)
// instead of raw shell.

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
	// Nor does a workspace with no Makefile, or no workspace at all.
	if got := makeTargetRemedy(t.TempDir(), "vuln"); got != "" {
		t.Errorf("makeTargetRemedy without a Makefile = %q, want empty", got)
	}
	if got := makeTargetRemedy("", "vuln"); got != "" {
		t.Errorf("makeTargetRemedy without a workspace = %q, want empty", got)
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
