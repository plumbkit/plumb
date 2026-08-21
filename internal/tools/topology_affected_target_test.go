package tools

import (
	"strings"
	"testing"
)

// TestTestTarget covers the two things PLAN-378 was filed for: a target is
// emitted only where the workspace's runner takes a positional path, and it is
// expressed relative to the directory that runner works in.
func TestTestTarget(t *testing.T) {
	goScope := TestScope{Language: "go", ScopedTests: true}
	subdirGo := TestScope{Language: "go", WorkingDir: "plumb", ScopedTests: true}

	cases := []struct {
		name  string
		scope TestScope
		dir   string
		want  string
		ok    bool
	}{
		{"go at the workspace root", goScope, "internal/config", "./internal/config/...", true},
		{"go, whole tree", goScope, ".", "./...", true},

		// The headline case: this repo indexes plumb/internal/config but
		// [tasks.go] working_dir = "plumb", so run_task runs from plumb/.
		{"go rebased onto working_dir", subdirGo, "plumb/internal/config", "./internal/config/...", true},
		{"go, dir IS the working_dir", subdirGo, "plumb", "./...", true},

		// A prefix that is not a path boundary must not match, or a sibling
		// directory would be silently rewritten into the wrong tree.
		{"sibling sharing a name prefix", subdirGo, "plumbkit/internal/x", "", false},
		{"outside the working_dir", subdirGo, "scripts", "", false},

		{"python takes a plain path", TestScope{Language: "python", ScopedTests: true}, "src/api", "src/api", true},
		{"python rebased", TestScope{Language: "python", WorkingDir: "svc", ScopedTests: true}, "svc/api", "api", true},

		// cargo test <filter> matches test NAMES, so a directory would silently
		// select nothing.
		{"rust scopes by name, not path", TestScope{Language: "rust", ScopedTests: true}, "src/api", "", false},
		// npm test ships with no {target} at all; run_task refuses a target.
		{"typescript has no positional target", TestScope{Language: "typescript"}, "src/api", "", false},
		{"go command without a placeholder", TestScope{Language: "go"}, "internal/config", "", false},
		{"no language attached", TestScope{}, "internal/config", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := testTarget(tc.scope, tc.dir)
			if ok != tc.ok {
				t.Fatalf("testTarget(%+v, %q) ok = %v, want %v (got %q)", tc.scope, tc.dir, ok, tc.ok, got)
			}
			if got != tc.want {
				t.Errorf("testTarget(%+v, %q) = %q, want %q", tc.scope, tc.dir, got, tc.want)
			}
		})
	}
}

// TestRunHeaderSuffix pins that an un-inferable command says so rather than
// printing a Go one. Emitting `go test` to a Python workspace is the defect
// PLAN-378 names.
func TestRunHeaderSuffix(t *testing.T) {
	if got := runHeaderSuffix(TestScope{Language: "go", ScopedTests: true}); !strings.Contains(got, "run_task") {
		t.Errorf("a path-scoped workspace should be pointed at run_task; got %q", got)
	}
	for _, scope := range []TestScope{
		{},
		{Language: "typescript"},
		{Language: "rust", ScopedTests: true},
	} {
		got := runHeaderSuffix(scope)
		if strings.Contains(got, "go test") {
			t.Errorf("scope %+v must not be told to run `go test`; got %q", scope, got)
		}
	}
}

// TestPackageRunLabel checks the per-row fallback, including the marker that
// explains why a directory outside working_dir has no target.
func TestPackageRunLabel(t *testing.T) {
	subdirGo := TestScope{Language: "go", WorkingDir: "plumb", ScopedTests: true}
	if got := packageRunLabel(subdirGo, "plumb/internal/config"); got != "./internal/config/..." {
		t.Errorf("in-tree row = %q, want the run_task target", got)
	}
	got := packageRunLabel(subdirGo, "scripts")
	if !strings.Contains(got, "scripts") || !strings.Contains(got, "outside") {
		t.Errorf("an out-of-tree row must name the directory and say why it has no target; got %q", got)
	}
	if strings.Contains(got, "./scripts/...") {
		t.Errorf("an out-of-tree row must not emit a target that would run from the wrong directory; got %q", got)
	}
	if got := packageRunLabel(TestScope{}, "internal/config"); got != "internal/config" {
		t.Errorf("with nothing known, the row is the bare directory; got %q", got)
	}
}
