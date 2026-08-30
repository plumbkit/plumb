package tools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/topology"
	goext "github.com/plumbkit/plumb/internal/topology/extractors/golang"
)

// reasonChangedForTest mirrors the unexported tools.reasonChanged label. This
// file is package tools_test, so it cannot reference the constant directly;
// duplicating the string here is deliberate, and makes a change to the
// user-visible wording show up as a test failure rather than passing silently.
const reasonChangedForTest = "changed package"

// TestTopologyAffected_ColocatedTests proves the recall booster: a sibling test
// that does NOT call the changed symbol (so no dependency edge connects them) is
// still flagged because it lives in the same directory.
func TestTopologyAffected_ColocatedTests(t *testing.T) {
	ws := t.TempDir()
	write := func(name, src string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(ws, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("demo.go", "package demo\n\nfunc HandleRequest() {}\n")
	// Deliberately does not call HandleRequest — only co-location can find it.
	write("demo_test.go", "package demo\n\nimport \"testing\"\n\nfunc TestUnrelated(t *testing.T) {}\n")

	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n1, _ := s.SymbolsInFile(context.Background(), filepath.Join(ws, "demo.go"))
		n2, _ := s.SymbolsInFile(context.Background(), filepath.Join(ws, "demo_test.go"))
		if len(n1) > 0 && len(n2) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	tool := tools.NewTopologyAffected(func() *topology.Store { return s })
	args, _ := json.Marshal(map[string]any{"symbols": []string{"HandleRequest"}})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "TestUnrelated") {
		t.Errorf("co-located test TestUnrelated should be flagged; got:\n%s", out)
	}
	// The reason is now stated as the relationship ("changed package") rather than
	// the mechanism ("co-located"), because that is what tells a caller whether to
	// trust the hit.
	if !strings.Contains(out, reasonChangedForTest) {
		t.Errorf("output should say why the package is implicated; got:\n%s", out)
	}
}

// TestTopologyAffected_MultipleColocatedTests is the regression for #87: when a
// seeded directory holds more than one test, all of them must surface. The bug
// was that TestsInDirs left Node.ID == 0 on every row, so the caller's
// g.seen[n.ID] dedup collapsed every co-located test onto key 0 and emitted only
// the first. Three tests across two sibling files must all appear.
func TestTopologyAffected_MultipleColocatedTests(t *testing.T) {
	ws := t.TempDir()
	write := func(name, src string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(ws, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("demo.go", "package demo\n\nfunc HandleRequest() {}\n")
	// None of these call HandleRequest — only co-location can find them.
	write("demo_test.go", "package demo\n\nimport \"testing\"\n\nfunc TestAlpha(t *testing.T) {}\n\nfunc TestBeta(t *testing.T) {}\n")
	write("more_test.go", "package demo\n\nimport \"testing\"\n\nfunc TestGamma(t *testing.T) {}\n")

	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n1, _ := s.SymbolsInFile(context.Background(), filepath.Join(ws, "demo.go"))
		n2, _ := s.SymbolsInFile(context.Background(), filepath.Join(ws, "demo_test.go"))
		n3, _ := s.SymbolsInFile(context.Background(), filepath.Join(ws, "more_test.go"))
		if len(n1) > 0 && len(n2) > 0 && len(n3) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	tool := tools.NewTopologyAffected(func() *topology.Store { return s })
	args, _ := json.Marshal(map[string]any{"symbols": []string{"HandleRequest"}})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"TestAlpha", "TestBeta", "TestGamma"} {
		if !strings.Contains(out, name) {
			t.Errorf("co-located test %s should be flagged (regression #87); got:\n%s", name, out)
		}
	}
}

// TestTopologyAffected_FileRootSeedsColocation proves the files: input path:
// a changed file resolved by its exact path (SymbolsInFile, not an FTS5
// path-string search) seeds its directory, so co-located sibling tests surface
// even though no dependency edge connects them.
func TestTopologyAffected_FileRootSeedsColocation(t *testing.T) {
	ws := t.TempDir()
	write := func(name, src string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(ws, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("demo.go", "package demo\n\nfunc HandleRequest() {}\n")
	write("demo_test.go", "package demo\n\nimport \"testing\"\n\nfunc TestUnrelated(t *testing.T) {}\n")

	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n1, _ := s.SymbolsInFile(context.Background(), filepath.Join(ws, "demo.go"))
		n2, _ := s.SymbolsInFile(context.Background(), filepath.Join(ws, "demo_test.go"))
		if len(n1) > 0 && len(n2) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	tool := tools.NewTopologyAffected(func() *topology.Store { return s })
	args, _ := json.Marshal(map[string]any{"files": []string{"demo.go"}})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "TestUnrelated") {
		t.Errorf("files input should surface co-located test TestUnrelated; got:\n%s", out)
	}
	if !strings.Contains(out, reasonChangedForTest) {
		t.Errorf("output should say why the package is implicated; got:\n%s", out)
	}
}

// TestTopologyAffected_TestsInDirs unit-checks the store query that backs the
// co-location booster: only tests whose immediate directory matches are returned.
func TestTopologyAffected_TestsInDirs(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"a_test.go":     "package a\n\nimport \"testing\"\n\nfunc TestTop(t *testing.T) {}\n",
		"sub/b_test.go": "package b\n\nimport \"testing\"\n\nfunc TestSub(t *testing.T) {}\n",
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := s.SymbolsInFile(context.Background(), filepath.Join(ws, "sub/b_test.go")); len(n) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Top-level directory is "." — only TestTop should match, not the subdir test.
	got, err := s.TestsInDirs(context.Background(), []string{"."})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, n := range got {
		names[n.Name] = true
	}
	if !names["TestTop"] {
		t.Errorf("TestsInDirs(\".\") should include TestTop; got %v", names)
	}
	if names["TestSub"] {
		t.Errorf("TestsInDirs(\".\") must not include the subdir TestSub; got %v", names)
	}
}

// TestSessionStart_TopologyLedGuidance verifies that when the topology index is
// active, the Claude Code guidance leads with topology (the Map) and names
// topology_affected as the headline post-change tool.
func TestSessionStart_TopologyLedGuidance(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module demo\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "demo.go"), []byte("package demo\n\nfunc Run() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	tool := tools.NewSessionStart(
		func(context.Context) string { return ws }, nil, nil,
		func() bool { return false },
		func() string { return "claude-code" },
		nil,
	).WithTopology(func() *topology.Store { return s })

	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Topology (the Map)", "topology_affected", "which tests to run"} {
		if !strings.Contains(out, want) {
			t.Errorf("topology-led guidance missing %q:\n%s", want, out)
		}
	}
}

// TestSessionStart_LSPLedGuidanceWhenTopologyOff verifies the fallback when no
// topology store is wired: the LSP-led list plus an enable-topology tip.
func TestSessionStart_LSPLedGuidanceWhenTopologyOff(t *testing.T) {
	ws := t.TempDir()
	tool := tools.NewSessionStart(
		func(context.Context) string { return ws }, nil, nil,
		func() bool { return false },
		func() string { return "claude-code" },
		nil,
	)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "workspace_symbols") {
		t.Errorf("LSP-led guidance should mention workspace_symbols:\n%s", out)
	}
	if !strings.Contains(out, "[topology] enabled = true") {
		t.Errorf("LSP-led guidance should tip enabling topology:\n%s", out)
	}
}

// editLaneWarningSubstrings are the load-bearing phrases the Claude Code
// edit-lane warning must carry: it must name the anti-pattern, the exact harness
// error an agent will have already seen, and the correct plumb tool.
var editLaneWarningSubstrings = []string{
	"Edit lane",
	"File has not been read yet",
	"File has been modified since read",
	"edit_file",
	"native",
}

// TestSessionStart_EditLaneWarning_ClaudeCode proves the warning is present for
// a Claude Code client in BOTH the topology-led and LSP-led guidance branches
// (it is written before the branch, so it must survive either path). This is
// the structural guard for the harness/plumb read-state mismatch fix.
func TestSessionStart_EditLaneWarning_ClaudeCode(t *testing.T) {
	newTool := func(ws string, topoOn bool) *tools.SessionStart {
		tool := tools.NewSessionStart(
			func(context.Context) string { return ws }, nil, nil,
			func() bool { return false },
			func() string { return "claude-code" },
			nil,
		)
		if topoOn {
			s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
				[]topology.Extractor{goext.New()})
			if err != nil {
				t.Fatalf("topology.Open: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })
			tool = tool.WithTopology(func() *topology.Store { return s })
		}
		return tool
	}

	for _, topoOn := range []bool{false, true} {
		name := "topology-off"
		if topoOn {
			name = "topology-on"
		}
		t.Run(name, func(t *testing.T) {
			ws := t.TempDir()
			if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module demo\n\ngo 1.22\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := newTool(ws, topoOn).Execute(context.Background(), json.RawMessage(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range editLaneWarningSubstrings {
				if !strings.Contains(out, want) {
					t.Errorf("edit-lane warning missing %q (topoOn=%v):\n%s", want, topoOn, out)
				}
			}
		})
	}
}

// TestSessionStart_EditLaneWarning_AbsentForDesktop proves the warning does NOT
// fire for Claude Desktop: Desktop has no native Edit tool, so the warning would
// be wrong (and the Desktop guidance already says all file ops go through plumb).
func TestSessionStart_EditLaneWarning_AbsentForDesktop(t *testing.T) {
	ws := t.TempDir()
	tool := tools.NewSessionStart(
		func(context.Context) string { return ws }, nil, nil,
		func() bool { return false },
		func() string { return "claude-ai" },
		nil,
	)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "## Tool guidance (Claude Desktop)") {
		t.Fatalf("expected Desktop guidance:\n%s", out)
	}
	if strings.Contains(out, "File has not been read yet") {
		t.Errorf("Desktop guidance must NOT carry the native-Edit warning:\n%s", out)
	}
}

// TestTopologyAffected_EveryChangedPackageIsNamed covers two properties of the
// multi-package answer, and deliberately does NOT claim to cover a third.
//
// Covered: when several files change across packages, every changed package is
// listed as a package to run AND has its tests named — naming only the first was
// arbitrary — and a complete answer carries no truncation banner, since a false
// "cut at max_results=50" above a two-package list is a wrong statement in the
// loudest position.
//
// NOT covered: the node-budget starvation this file was extended for. max_results
// was also spent as a MaxNodes budget, so a root with a large traversal consumed
// it and later roots' directories went unseeded — the wider the fan-out, the
// fewer packages reported. Reproducing that needs one root whose inward traversal
// exceeds max_results, which in Go means cross-file import edges, and those do not
// materialise in a t.TempDir() fixture here even after waiting on the indexer.
// The fix is verified against the real index instead: a change to
// internal/config/config.go returned 2 of 9 affected packages before and all 9
// after, with internal/tools (1,312 tests) among those restored.
//
// Left explicit rather than papered over: a synthetic fixture that cannot fill
// the budget passes against the bug, which is exactly how the earlier collision
// test missed it.
func TestTopologyAffected_EveryChangedPackageIsNamed(t *testing.T) {
	ws := t.TempDir()
	write := func(name, src string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(ws, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// "aaa" sorts first, so it is the root walked first. 200 declarations put its
	// traversal far past the default max_results of 50.
	var big strings.Builder
	big.WriteString("package aaa\n")
	for i := range 200 {
		fmt.Fprintf(&big, "\nfunc Big%03d() int { return %d }\n", i, i)
	}
	write("aaa/aaa.go", big.String())
	write("aaa/aaa_test.go",
		"package aaa\n\nimport \"testing\"\n\nfunc TestBig(t *testing.T) {}\n")

	// The second changed file, in a different package, with the test that must
	// survive the first root's traversal.
	write("zzz/zzz.go", "package zzz\n\nfunc Small() int { return 1 }\n")
	write("zzz/zzz_test.go",
		"package zzz\n\nimport \"testing\"\n\nfunc TestSmallSurvives(t *testing.T) { _ = Small() }\n")

	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		a, _ := s.SymbolsInFile(context.Background(), filepath.Join(ws, "aaa/aaa.go"))
		z, _ := s.SymbolsInFile(context.Background(), filepath.Join(ws, "zzz/zzz_test.go"))
		if len(a) > 100 && len(z) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	tool := tools.NewTopologyAffected(func() *topology.Store { return s }).
		WithTestScope(func() tools.TestScope {
			return tools.TestScope{Language: "go", Style: tools.TargetGoPackage}
		})
	// No max_results: the shipped default is the behaviour under test.
	args, _ := json.Marshal(map[string]any{
		"files": []string{
			filepath.Join(ws, "aaa/aaa.go"),
			filepath.Join(ws, "zzz/zzz.go"),
		},
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "TestSmallSurvives") {
		t.Errorf("every changed package must have its tests named, not just the first:\n%s", out)
	}
	if !strings.Contains(out, "TestBig") {
		t.Errorf("the first changed package's tests must still be named:\n%s", out)
	}
	if !strings.Contains(out, "./zzz/...") {
		t.Errorf("package zzz must be listed as a package to run:\n%s", out)
	}
	// And the banner must not claim a cut that did not happen: two packages is
	// nowhere near max_results, so announcing a truncation would be a false
	// statement in the loudest position on the response.
	if strings.Contains(out, "TRUNCATED") {
		t.Errorf("a complete answer must not carry a truncation banner:\n%s", out)
	}
}

// TestTopologyAffected_ImportNameCollisionDoesNotDragInUnrelatedPackage is the
// regression for the recall bug found while measuring docs/use-cases.md.
//
// SymbolsInFile returns every node in a file, including one per `import` — named
// for the imported package ("strings"), not for anything the edit touched. Those
// were seeded as traversal roots, and fromGraph then passed root.Name to
// store.Impact, which re-resolved the bare name against the whole index with no
// tie-break. So "strings" landed on an arbitrary import node in an unrelated
// package, whose directory was then seeded for co-location, dumping that
// package's entire test suite into the answer.
//
// Two symptoms, both asserted here, because fixing only the first would leave
// the damaging one in place:
//
//   - precision: the unrelated package's tests must not appear at all;
//   - recall: the changed file's OWN test must appear at the DEFAULT max_results.
//     In the real index the flood pushed it past the 50-result cut entirely, so an
//     agent running the suggested tests would not have run the one test that
//     covered the function it just edited.
func TestTopologyAffected_ImportNameCollisionDoesNotDragInUnrelatedPackage(t *testing.T) {
	ws := t.TempDir()
	write := func(name, src string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(ws, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Directory names matter. The index is walked in path order, so "aaa" is
	// indexed before "zzz" and its `import "strings"` node gets the lower rowid —
	// which is exactly the row an unordered `LIMIT 1` returns. Name the unrelated
	// package second and the buggy lookup happens to pick the right file, and this
	// test passes against the bug it exists to catch.
	//
	// An unrelated package that also imports "strings", carrying enough tests to
	// swamp the default result window if it is wrongly implicated.
	write("aaa/noise.go",
		"package aaa\n\nimport \"strings\"\n\nfunc Noise(s string) string { return strings.ToUpper(s) }\n")
	var noise strings.Builder
	noise.WriteString("package aaa\n\nimport \"testing\"\n")
	for i := range 60 {
		fmt.Fprintf(&noise, "\nfunc TestNoise%02d(t *testing.T) {}\n", i)
	}
	write("aaa/noise_test.go", noise.String())

	// The changed package. It imports "strings", which is the collision seed.
	write("zzz/target.go",
		"package zzz\n\nimport \"strings\"\n\nfunc Target(s string) string { return strings.TrimSpace(s) }\n")
	write("zzz/target_test.go",
		"package zzz\n\nimport \"testing\"\n\nfunc TestTarget(t *testing.T) { _ = Target(\" x \") }\n")

	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		a, _ := s.SymbolsInFile(context.Background(), filepath.Join(ws, "zzz/target.go"))
		b, _ := s.SymbolsInFile(context.Background(), filepath.Join(ws, "zzz/target_test.go"))
		c, _ := s.SymbolsInFile(context.Background(), filepath.Join(ws, "aaa/noise_test.go"))
		if len(a) > 0 && len(b) > 0 && len(c) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	tool := tools.NewTopologyAffected(func() *topology.Store { return s })
	// No max_results: the default (50) is the shipped behaviour under test.
	args, _ := json.Marshal(map[string]any{
		"files": []string{filepath.Join(ws, "zzz/target.go")},
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(out, "TestNoise") {
		t.Errorf("unrelated package implicated via the shared import name \"strings\";"+
			" its tests must not appear:\n%s", out)
	}
	if !strings.Contains(out, "TestTarget") {
		t.Errorf("the changed file's own co-located test must appear at the DEFAULT"+
			" max_results, not only when the caller raises it:\n%s", out)
	}
}

// defaultMaxResults reads the shipped max_results default out of the tool's own
// input schema.
//
// It was a duplicated literal (50) whose comment claimed a change to the default
// would surface here. It would not: an adversarial review raised the default to
// 100 AND restored the pre-fix bug, and the fixture-size guard still passed
// (81 > 50 held), so every assertion went vacuous — the identical failure that
// blinded the two previous tests in this area, merely deferred to a future
// bump. Reading the real value means raising the default above the fixture's
// reach trips the guard instead of silently disarming it.
func defaultMaxResults(t *testing.T) int {
	t.Helper()
	var schema struct {
		Properties struct {
			MaxResults struct {
				Default int `json:"default"`
			} `json:"max_results"`
		} `json:"properties"`
	}
	tool := tools.NewTopologyAffected(nil)
	if err := json.Unmarshal(tool.InputSchema(), &schema); err != nil {
		t.Fatalf("parsing topology_affected input schema: %v", err)
	}
	if n := schema.Properties.MaxResults.Default; n > 0 {
		return n
	}
	t.Fatal("topology_affected's schema declares no max_results default; this test " +
		"sizes its fixture from it and cannot run blind")
	return 0
}

// indexSettleTimeout bounds the wait for the indexer to produce cross-file
// edges. Generous on purpose: the fixture indexes in ~2s locally, so a timeout
// means something is wrong rather than merely slow.
const indexSettleTimeout = 30 * time.Second

// TestTopologyAffected_WideFanOutReportsEveryImporter is the synthetic
// regression that 7568173c shipped without, and the reason PLAN-384 existed.
//
// The bug: max_results is documented as bounding PACKAGES, but it was also spent
// as a node budget — passed to ImpactFrom as MaxNodes, with the root loop
// breaking once dependents+tests reached it. A widely-imported package therefore
// exhausted the budget inside its first root's traversal and later importer
// directories went unseeded, so the WIDER the fan-out, the FEWER dependents were
// reported. Measured in production: internal/config/config.go returned 2 of its
// 9 packages, silently dropping internal/tools and its 1,312 tests.
//
// Two fixture properties are load-bearing, and both were missing from the
// earlier attempts that left this untested. Do not shrink either one:
//
//   - Importer directories are TWO segments deep. matchImportDir refuses a
//     suffix shorter than minImportSegments (2) so that `import "strings"` cannot
//     bind to a local strings/ directory. A fixture whose packages sit one level
//     deep gets no import edges at all, and every assertion here passes vacuously.
//   - Inward NODES must exceed max_results while PACKAGES stay under it. That is
//     what separates the two cuts: fromColocation legitimately caps packages at
//     the same number, so a fixture that raises both together truncates under the
//     fix as well and passes against the bug. Hence 20 packages of 4 files each:
//     ~81 inward nodes, 21 packages.
//
// Confirmed red against the restored pre-fix behaviour: 14 packages reported
// instead of 21, 7 of 20 importers missing (imp13..imp19), plus a false
// truncation banner. Each arm was also mutated alone:
//
//   - MaxNodes: g.maxResults — caught.
//   - the g.total() >= g.maxResults early return in fromGraph — caught, and it
//     fires BOTH assertions (missing importers and the false banner).
//   - the `if g.truncated { break }` in collectAffected's root loop — NOT
//     caught, and no fixture can catch it. g.truncated is set only in
//     fromColocation, which runs after that loop, so in isolation the break can
//     never fire: it is an equivalent mutant, live only in combination with the
//     early return above. Do not grow the fixture chasing it — an earlier review
//     read this gap as missing coverage, which it is not.
func TestTopologyAffected_WideFanOutReportsEveryImporter(t *testing.T) {
	const (
		importerPkgs = 20
		filesPerPkg  = 4
	)

	ws := t.TempDir()
	write := func(name, src string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(ws, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// go.mod gives the imports a module prefix to strip, exercising the same
	// suffix match production uses.
	write("go.mod", "module example.com/m\n\ngo 1.22\n")
	write("internal/target/target.go", "package target\n\nfunc Target() int { return 1 }\n")
	write("internal/target/target_test.go",
		"package target\n\nimport \"testing\"\n\nfunc TestTarget(t *testing.T) {}\n")

	for i := range importerPkgs {
		dir := fmt.Sprintf("internal/imp%02d", i)
		for f := range filesPerPkg {
			write(fmt.Sprintf("%s/use%d.go", dir, f), fmt.Sprintf(
				"package imp%02d\n\nimport \"example.com/m/internal/target\"\n\n"+
					"func Use%02d_%d() int { return target.Target() }\n", i, i, f))
		}
		write(dir+"/use_test.go", fmt.Sprintf(
			"package imp%02d\n\nimport \"testing\"\n\nfunc TestUse%02d(t *testing.T) {}\n", i, i))
	}

	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Wait on the EDGES, not the nodes. linkImports runs at the END of an index
	// pass, so a file's symbols become queryable before its import edges exist —
	// polling for nodes is what made an earlier attempt conclude, wrongly, that
	// fixtures cannot produce cross-file edges at all.
	resultCap := defaultMaxResults(t)
	var inward int
	deadline := time.Now().Add(indexSettleTimeout)
	for time.Now().Before(deadline) {
		nodes, _ := s.SymbolsInFile(context.Background(), filepath.Join(ws, "internal/target/target.go"))
		for _, n := range nodes {
			if n.Kind != topology.KindFunction {
				continue
			}
			nb, nerr := s.ImpactFrom(context.Background(), n, topology.ImpactOpts{
				Depth: 2, MaxNodes: 2000, MaxBytes: 100000,
				EdgeKinds: []string{"calls", "imports", "contains"},
			})
			if nerr == nil && len(nb.DependedOnBy.Nodes) > inward {
				inward = len(nb.DependedOnBy.Nodes)
			}
		}
		if inward > resultCap {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The fixture-size guard. A fixture too small to fill the budget passes
	// against the bug it was written for — that has now happened twice in this
	// area, so it fails loudly here instead.
	//
	// The message distinguishes the two ways to get here, because they need
	// opposite fixes and the wrong one wastes the next person's time: zero inward
	// nodes means the index never settled (a slow machine, or import edges not
	// being produced at all), while a non-zero count below the cap means the
	// fixture genuinely shrank.
	if inward == 0 {
		t.Fatalf("no inward nodes after %s — the index never settled, so this test "+
			"proved nothing. Check that linkImports still produces cross-file edges "+
			"(it runs at the END of an index pass) before touching the fixture size",
			indexSettleTimeout)
	}
	if inward <= resultCap {
		t.Fatalf("fixture too small to exercise the cap: %d inward nodes, need > %d "+
			"(the shipped max_results default). This test cannot detect the regression "+
			"it exists for; grow importerPkgs/filesPerPkg rather than relaxing this "+
			"check", inward, resultCap)
	}

	tool := tools.NewTopologyAffected(func() *topology.Store { return s })
	// No max_results: the shipped default is the behaviour under test.
	args, _ := json.Marshal(map[string]any{
		"files": []string{filepath.Join(ws, "internal/target/target.go")},
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}

	var missing []string
	for i := range importerPkgs {
		if dir := fmt.Sprintf("internal/imp%02d", i); !strings.Contains(out, dir) {
			missing = append(missing, dir)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d of %d importer packages went unreported (%s); a wide fan-out must "+
			"not starve later importers of the traversal budget:\n%s",
			len(missing), importerPkgs, strings.Join(missing, ", "), out)
	}

	// A complete answer must not claim a cut. The banner is the loudest line on
	// the response, and pointing a reader at max_results when nothing was cut is
	// its own defect.
	if strings.Contains(out, "TRUNCATED") || strings.Contains(out, "[truncated:") {
		t.Errorf("a complete answer must not carry a truncation banner:\n%s", out)
	}
}

func TestTopologyAffected_IncludesAdmittedCrossFileCallers(t *testing.T) {
	ws := t.TempDir()
	write := func(rel, src string) {
		t.Helper()
		path := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/target/target.go", "package target\n\nfunc Target() {}\n")
	write("internal/caller/caller.go", "package caller\n\nimport \"example.com/project/internal/target\"\n\nfunc Use() { target.Target() }\n")
	write("internal/caller/caller_test.go", "package caller\n\nimport \"testing\"\n\nfunc TestCaller(t *testing.T) {}\n")

	store, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024}, []topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	var target topology.Node
	for time.Now().Before(deadline) {
		nodes, _ := store.SymbolsInFile(ctx, filepath.Join(ws, "internal/target/target.go"))
		for _, n := range nodes {
			if n.Name != "Target" || n.Kind != topology.KindFunction {
				continue
			}
			target = n
			res, rerr := store.ImpactFrom(ctx, n, topology.ImpactOpts{Depth: 1, MaxNodes: 50, MaxBytes: 50000, EdgeKinds: []string{"calls"}, IncludeDerivedCalls: true})
			if rerr == nil {
				for _, e := range res.DependedOnBy.Edges {
					if e.Source == "call-resolver" {
						goto settled
					}
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
settled:
	if target.ID == 0 {
		t.Fatal("target function was not indexed")
	}
	defaultRes, err := store.ImpactFrom(ctx, target, topology.ImpactOpts{Depth: 1, MaxNodes: 50, MaxBytes: 50000, EdgeKinds: []string{"calls"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range defaultRes.DependedOnBy.Edges {
		if e.Source == "call-resolver" {
			t.Fatal("default impact exposed a derived caller")
		}
	}

	tool := tools.NewTopologyAffected(func() *topology.Store { return store })
	args, _ := json.Marshal(map[string]any{"symbols": []string{"Target"}})
	out, err := tool.Execute(ctx, args)
	if err != nil {
		t.Fatalf("topology_affected: %v", err)
	}
	if !strings.Contains(out, "internal/caller") || !strings.Contains(out, "1 tests") {
		t.Errorf("admitted cross-file caller package/test count missing from affected output:\n%s", out)
	}
}
