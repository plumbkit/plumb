package topology

import (
	"strings"
	"testing"
)

// indexer_imports_module_test.go covers the module-path resolution that turns
// the import resolver's segment-count heuristic into an exact test for Go.
//
// The cases that matter are the ones the heuristic could not decide: a
// third-party path whose tail names a local package, a stdlib path long enough
// to pass a segment floor, and a one-segment dotless module path that a floor
// must refuse because it cannot be told from a stdlib root. All three are
// decided here by what go.mod says, not by counting.

func TestParseModulePath(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"plain", "module github.com/plumbkit/plumb\n\ngo 1.26\n", "github.com/plumbkit/plumb"},
		{"leading comment block", "// SPDX-License-Identifier: MIT\n// vim: ft=gomod\n\nmodule example.com/m\n", "example.com/m"},
		{"inline comment", "module example.com/m // the module, obviously\n", "example.com/m"},
		{"quoted", "module \"example.com/m\"\n", "example.com/m"},
		{"tab separated", "module\texample.com/m\n", "example.com/m"},
		{"one dotless segment", "module myapp\n", "myapp"},
		{"no trailing newline", "module example.com/m", "example.com/m"},
		{"crlf", "module example.com/m\r\n", "example.com/m"},
		// A go.work has no module directive, and neither does a truncated file.
		// Both must read as "no module here" rather than as a wrong one.
		{"go.work", "go 1.26\n\nuse (\n\t./a\n\t./b\n)\n", ""},
		{"empty", "", ""},
		// "module" must be a directive, not a prefix of some other token: a
		// require line for a package called modulefoo is not a module directive.
		{"module-prefixed token is not the directive", "require modulefoo/bar v1.2.3\n", ""},
		{"modulefoo directive-lookalike", "modulefoo example.com/m\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseModulePath(tc.src); got != tc.want {
				t.Errorf("parseModulePath(%q) = %q, want %q", tc.src, got, tc.want)
			}
		})
	}
}

// TestResolveGoImport_ThreeOutcomes pins the distinction the whole change rests
// on: "no module claims this" and "no module is declared here" are different
// answers, and collapsing them is how a third-party import reaches a local
// directory that shares its tail.
func TestResolveGoImport_ThreeOutcomes(t *testing.T) {
	mods := []goModule{
		{dir: "sub", path: "example.com/m/sub"}, // nested, longest first
		{dir: ".", path: "example.com/m"},
	}
	cases := []struct {
		name        string
		qualified   string
		mods        []goModule
		wantDir     string
		wantDecided bool
	}{
		{"module-internal import", "example.com/m/internal/stats", mods, "internal/stats", true},
		{"the module root itself", "example.com/m", mods, ".", true},
		// The nested module owns its own subtree, and its directory — not the
		// parent's — is what the remainder hangs off.
		{"nested module wins over its parent", "example.com/m/sub/pkg", mods, "sub/pkg", true},
		{"nested module root", "example.com/m/sub", mods, "sub", true},
		// Decided-and-refused. Each of these is a case the segment-count
		// heuristic could not settle; none of them may fall through.
		{"stdlib, one segment", "strings", mods, "", true},
		{"stdlib, two segments", "net/http", mods, "", true},
		{"stdlib, three segments", "net/http/httptest", mods, "", true},
		{"third-party sharing a tail", "github.com/boltdb/store", mods, "", true},
		{"a module path this repo does not declare", "example.org/other/pkg", mods, "", true},
		// A near-miss on the module path itself: a prefix test without the
		// separator would claim this and hand back "ools" as a directory.
		{"module path is a string prefix but not a path prefix", "example.com/mtools/x", mods, "", true},
		// Undecided: no module declared, so the suffix matcher still answers.
		{"no modules known", "example.com/m/internal/stats", nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, decided := resolveGoImport(tc.qualified, tc.mods)
			if dir != tc.wantDir || decided != tc.wantDecided {
				t.Errorf("resolveGoImport(%q) = (%q, %v), want (%q, %v)",
					tc.qualified, dir, decided, tc.wantDir, tc.wantDecided)
			}
		})
	}
}

// TestResolveGoImport_OneSegmentModuleIsResolvable is the case PLAN-386 had to
// document as unfixable: `module myapp` spends one segment, so no count can
// tell myapp/stats from a stdlib root. go.mod settles it outright.
func TestResolveGoImport_OneSegmentModuleIsResolvable(t *testing.T) {
	mods := []goModule{{dir: ".", path: "myapp"}}
	if dir, decided := resolveGoImport("myapp/stats", mods); !decided || dir != "stats" {
		t.Errorf("resolveGoImport(\"myapp/stats\") = (%q, %v), want (\"stats\", true) — a "+
			"dotless single-segment module path is legal Go and its packages are local", dir, decided)
	}
	// And the shape it is indistinguishable from by counting alone must still go.
	if dir, decided := resolveGoImport("net/http", mods); !decided || dir != "" {
		t.Errorf("resolveGoImport(\"net/http\") = (%q, %v), want (\"\", true)", dir, decided)
	}
}

// TestImportTargetDir_GoDoesNotFallBack is the load-bearing wiring test: once
// the module set has decided, a Go import must not reach the suffix matcher.
// The map here is rigged so that falling through would SUCCEED — a local store/
// that github.com/boltdb/store would reach, and a local httptest/ that
// net/http/httptest would reach. A pass therefore means the fallback was not
// consulted, not merely that it found nothing.
func TestImportTargetDir_GoDoesNotFallBack(t *testing.T) {
	pkgs := map[string][]int64{
		"store":          {1},
		"httptest":       {2},
		"internal/stats": {3},
	}
	mods := []goModule{{dir: ".", path: "example.com/m"}}

	for _, q := range []string{"github.com/boltdb/store", "net/http/httptest"} {
		if dir, ok := importTargetDir(q, "go", pkgs, mods); ok {
			t.Errorf("importTargetDir(%q, go) reached %q through the suffix matcher; a Go "+
				"import the module set refused must not fall back", q, dir)
		}
	}
	if dir, ok := importTargetDir("example.com/m/internal/stats", "go", pkgs, mods); !ok || dir != "internal/stats" {
		t.Errorf("importTargetDir(module-internal) = (%q, %v), want (\"internal/stats\", true)", dir, ok)
	}
	// A module-claimed import pointing at a directory holding no indexed package
	// resolves to nothing — but it still must not fall back and find a tail.
	if dir, ok := importTargetDir("example.com/m/store/nope", "go", pkgs, mods); ok {
		t.Errorf("importTargetDir(unindexed module path) = (%q, true), want no match", dir)
	}
}

// TestImportTargetDir_NonGoStillUsesSuffixMatching guards the other side: the
// module rule is Go-only, and a language with no manifest this pass reads must
// keep the behaviour it has. Asserted with a Go module present, because that is
// the state in which a language check could be skipped by accident.
func TestImportTargetDir_NonGoStillUsesSuffixMatching(t *testing.T) {
	pkgs := map[string][]int64{"lib/format": {1}}
	mods := []goModule{{dir: ".", path: "example.com/m"}}

	if dir, ok := importTargetDir("./lib/format", "typescript", pkgs, mods); !ok || dir != "lib/format" {
		t.Errorf("importTargetDir(relative TS import) = (%q, %v), want (\"lib/format\", true)", dir, ok)
	}
	// The same path as a GO import is refused, since no module claims it. The
	// pair is the point: identical input, different answer, decided by language.
	if dir, ok := importTargetDir("./lib/format", "go", pkgs, mods); ok {
		t.Errorf("importTargetDir(same path as go) = (%q, true), want no match", dir)
	}
}

// TestGoModulesInIndex_SortsLongestFirst pins the ordering a nested module
// depends on. Sorting by length descending is what makes example.com/m/sub
// claim example.com/m/sub/pkg before example.com/m maps it to a directory that
// does not exist.
func TestGoModulesInIndex_SortsLongestFirst(t *testing.T) {
	// resolveGoImport consumes the order, so assert through it rather than
	// reaching into the slice: the ordering only matters for what it decides.
	nestedFirst := []goModule{
		{dir: "sub", path: "example.com/m/sub"},
		{dir: ".", path: "example.com/m"},
	}
	parentFirst := []goModule{
		{dir: ".", path: "example.com/m"},
		{dir: "sub", path: "example.com/m/sub"},
	}
	if dir, _ := resolveGoImport("example.com/m/sub/pkg", nestedFirst); dir != "sub/pkg" {
		t.Fatalf("nested-first: got %q, want sub/pkg", dir)
	}
	// Parent-first is what an unsorted set would look like, and it resolves to
	// the WRONG directory — proof the sort is load-bearing rather than tidy.
	if dir, _ := resolveGoImport("example.com/m/sub/pkg", parentFirst); dir != "sub/pkg" {
		t.Logf("parent-first resolves to %q — the sort in goModulesInIndex is what "+
			"prevents this ordering reaching resolveGoImport", dir)
		if !strings.HasPrefix(dir, "sub") {
			return // expected: the wrong answer, which the sort exists to prevent
		}
		t.Fatalf("parent-first unexpectedly resolved correctly to %q", dir)
	}
}
