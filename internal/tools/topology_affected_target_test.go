package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTestTarget covers what PLAN-378 was filed for: a target is emitted only
// where one can be spelled correctly, and it is expressed relative to the
// directory the test command runs in.
func TestTestTarget(t *testing.T) {
	goScope := TestScope{Language: "go", Style: TargetGoPackage}
	subdirGo := TestScope{Language: "go", WorkingDir: "plumb", Style: TargetGoPackage}

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

		{"python takes a plain path", TestScope{Language: "python", Style: TargetPath}, "src/api", "./src/api", true},
		{"python rebased", TestScope{Language: "python", WorkingDir: "svc", Style: TargetPath}, "svc/api", "./api", true},
		{"python whole tree stays bare", TestScope{Language: "python", Style: TargetPath}, ".", ".", true},

		// targetPattern admits "-" in any position, so a directory called "-x"
		// would otherwise reach pytest AS THE FLAG -x: the whole suite in
		// exit-first mode, silently, instead of the one package meant.
		{"dash-leading dir cannot become a flag", TestScope{Language: "python", Style: TargetPath}, "-x", "./-x", true},
		{"dash-leading dir, go", TestScope{Language: "go", Style: TargetGoPackage}, "-x", "./-x/...", true},

		// run_task bounds a target to one shell-safe argument. A directory it
		// would refuse must not be emitted as a target by the tool telling the
		// caller to pass it there.
		{"space in the path", goScope, "my pkg/sub", "", false},
		{"parenthesised path", TestScope{Language: "python", Style: TargetPath}, "src/api (v2)", "", false},
		{"plus in the path", goScope, "pkg+ext/x", "", false},

		{"no style resolved", TestScope{Language: "rust"}, "src/api", "", false},
		{"nothing known", TestScope{}, "internal/config", "", false},
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

// TestTestTargetIsAlwaysRunTaskSafe is the invariant behind the row: whatever
// testTarget emits, run_task's own validator must accept. Asserting the
// property rather than a list of cases is what makes it hold for directory
// shapes nobody thought to enumerate.
func TestTestTargetIsAlwaysRunTaskSafe(t *testing.T) {
	dirs := []string{
		"internal/config", ".", "plumb/internal/x", "a b/c", "x+y", "sr c/api (v2)",
		"weird'quote", "tab\tsep", "emoji🙂/pkg", "trailing/", "../escape", "deep/a/b/c/d",
		"-x", "--flag", "-", "..",
	}
	scopes := []TestScope{
		{Language: "go", Style: TargetGoPackage},
		{Language: "go", WorkingDir: "plumb", Style: TargetGoPackage},
		{Language: "python", Style: TargetPath},
		{Language: "python", WorkingDir: "svc", Style: TargetPath},
	}
	for _, scope := range scopes {
		for _, dir := range dirs {
			target, ok := testTarget(scope, dir)
			if !ok {
				continue
			}
			if !targetPattern.MatchString(target) {
				t.Errorf("testTarget(%+v, %q) emitted %q, which run_task refuses "+
					"(targetPattern %s)", scope, dir, target, targetPattern)
			}
		}
	}
}

// TestRunHeaderSuffix asserts what each branch SAYS, positively.
//
// It used to assert only that the header did not contain "go test" — a string
// that appears nowhere in this file, so no code path could produce it and the
// assertion could never fail. An adversarial review proved it by making every
// scope return the run_task header (so a rust or no-language workspace is told
// to hand a directory to run_task, the exact PLAN-378 defect class) and
// watching the whole suite stay green. Negative assertions about absent
// literals are not tests.
func TestRunHeaderSuffix(t *testing.T) {
	cases := []struct {
		name  string
		scope TestScope
		want  []string
		deny  []string
	}{
		{
			name:  "a spellable target points at run_task",
			scope: TestScope{Language: "go", Style: TargetGoPackage},
			want:  []string{"run_task", `slot:"test"`, "target"},
		},
		{
			name:  "no language attached says so",
			scope: TestScope{},
			want:  []string{"no language is attached", "your own test runner"},
			deny:  []string{"run_task"},
		},
		{
			name:  "a name-scoped runner is named and not commanded",
			scope: TestScope{Language: "rust"},
			want:  []string{"rust", "could not be narrowed to a directory"},
			deny:  []string{"run_task", "takes no"},
		},
		{
			name:  "a flag-scoped runner is named and not commanded",
			scope: TestScope{Language: "typescript"},
			want:  []string{"typescript", "could not be narrowed to a directory"},
			deny:  []string{"run_task", "takes no"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runHeaderSuffix(tc.scope)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("header for %+v must mention %q; got %q", tc.scope, w, got)
				}
			}
			for _, d := range tc.deny {
				if strings.Contains(got, d) {
					t.Errorf("header for %+v must NOT mention %q — that workspace cannot "+
						"use the target it would be promised; got %q", tc.scope, d, got)
				}
			}
		})
	}
}

// TestPackageRunLabel checks the per-row fallback, including the marker that
// explains why a directory outside working_dir has no target.
func TestPackageRunLabel(t *testing.T) {
	subdirGo := TestScope{Language: "go", WorkingDir: "plumb", Style: TargetGoPackage}
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

// TestSchemaDefaultMatchesRuntimeDefault pins the two places max_results'
// default is written.
//
// This exists because of a defect an adversarial review found in the fix for a
// PREVIOUS review round. The e2e fixture sizes itself by reading the default out
// of the tool's InputSchema, which was meant to stop a future default bump
// silently disarming it. But the cap that actually shapes the answer is the
// literal in parseTopologyAffectedArgs, and nothing tied the two: raise the
// runtime one, leave the schema's documentation alone, and the fixture measures
// against a stale number while passing vacuously over a restored regression.
// Proven at the time by doing exactly that — the test went green over the bug.
//
// Reading the schema is only legitimate while this passes.
func TestSchemaDefaultMatchesRuntimeDefault(t *testing.T) {
	var schema struct {
		Properties struct {
			MaxResults struct {
				Default int `json:"default"`
			} `json:"max_results"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(topologyAffectedSchema, &schema); err != nil {
		t.Fatalf("parsing topologyAffectedSchema: %v", err)
	}
	if got := schema.Properties.MaxResults.Default; got != defaultMaxPackages {
		t.Errorf("schema advertises max_results default %d but the runtime applies %d. "+
			"Change BOTH: the e2e fixture sizes itself from the schema, so a mismatch "+
			"leaves it measuring against a cap that is not in force and passing over "+
			"regressions it exists to catch", got, defaultMaxPackages)
	}

	// And the default must actually be applied, not merely documented.
	a, err := parseTopologyAffectedArgs(json.RawMessage(`{"files":["x.go"]}`))
	if err != nil {
		t.Fatalf("parseTopologyAffectedArgs: %v", err)
	}
	if a.MaxResults != defaultMaxPackages {
		t.Errorf("an unspecified max_results resolved to %d, want %d", a.MaxResults, defaultMaxPackages)
	}
}
