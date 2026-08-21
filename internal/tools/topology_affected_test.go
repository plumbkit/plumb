package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

func TestTopologyAffected_NilStore(t *testing.T) {
	tool := NewTopologyAffected(func() *topology.Store { return nil })
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"symbols":["foo"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "disabled") {
		t.Errorf("expected 'disabled' message, got: %s", out)
	}
}

func TestTopologyAffected_NoInputs(t *testing.T) {
	tool := NewTopologyAffected(func() *topology.Store { return nil })
	_, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Error("expected error when no files or symbols given")
	}
}

func TestTopologyAffected_Defaults(t *testing.T) {
	a, err := parseTopologyAffectedArgs(json.RawMessage(`{"symbols":["foo"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if a.MaxResults != 50 {
		t.Errorf("max_results default=%d, want 50", a.MaxResults)
	}
}

func TestTopologyAffected_FormatNilResult(t *testing.T) {
	a := topologyAffectedArgs{Symbols: []string{"foo"}, MaxResults: 50}
	out := formatAffectedResult(nil, a)
	if strings.Contains(out, "disabled") {
		t.Errorf("nil result is a not-found case, not 'disabled'; got: %s", out)
	}
	if !strings.Contains(out, "none of the given files or symbols are in the index") {
		t.Errorf("expected not-found message, got: %s", out)
	}
}

func TestTopologyAffected_FormatEmptyResult(t *testing.T) {
	a := topologyAffectedArgs{Symbols: []string{"foo"}, MaxResults: 50}
	result := &affectedResult{}
	out := formatAffectedResult(result, a)
	if !strings.Contains(out, "none") {
		t.Errorf("empty result should say 'none', got: %s", out)
	}
}

func TestIncidentConfidence(t *testing.T) {
	edges := []topology.Edge{
		{FromID: 1, ToID: 2, Confidence: 0.8},
		{FromID: 2, ToID: 3, Confidence: 1.0},
	}
	m := incidentConfidence(edges)
	if m[2] != 1.0 {
		t.Errorf("node 2 incident confidence = %v, want 1.0 (max of 0.8, 1.0)", m[2])
	}
	if m[1] != 0.8 {
		t.Errorf("node 1 incident confidence = %v, want 0.8", m[1])
	}
}

func TestTopologyAffected_FormatSurfacesPackagesAndRecall(t *testing.T) {
	a := topologyAffectedArgs{Symbols: []string{"Foo"}, MaxResults: 50}
	result := &affectedResult{
		Tests: []affectedTest{
			{Node: topology.Node{Name: "TestFoo", Path: "internal/stats/foo_test.go", StartLine: 10}, Confidence: 0.5, Reason: reasonChanged},
			{Node: topology.Node{Name: "TestBar", Path: "internal/cli/bar_test.go", StartLine: 5}, Confidence: 0.5, Reason: reasonImporter},
		},
	}
	out := formatAffectedResult(result, a)
	// The actionable unit is the package: a caller narrows `go test` by path, not
	// by test name, so the runnable command is what the output leads with.
	for _, want := range []string{
		"go test ./internal/stats/...",
		"go test ./internal/cli/...",
		reasonChanged,
		reasonImporter,
		"biased toward recall",
		"TestFoo", // the changed package's tests are still named
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// The changed package sorts first even though it was not the largest.
	if strings.Index(out, "internal/stats") > strings.Index(out, "internal/cli") {
		t.Errorf("the changed package must be listed first:\n%s", out)
	}
}

// TestTopologyAffected_FormatAggregatesRatherThanEnumerates pins the property
// that motivated the change: a package reached only by import must contribute
// ONE line, not one line per test. Enumerating them made a one-line edit return
// 298 KB of near-identical rows.
func TestTopologyAffected_FormatAggregatesRatherThanEnumerates(t *testing.T) {
	a := topologyAffectedArgs{Symbols: []string{"Foo"}, MaxResults: 50}
	result := &affectedResult{
		Tests: []affectedTest{
			{Node: topology.Node{Name: "TestSeed", Path: "internal/stats/a_test.go", StartLine: 1}, Confidence: 0.5, Reason: reasonChanged},
		},
	}
	for i := range 500 {
		result.Tests = append(result.Tests, affectedTest{
			Node:       topology.Node{Name: fmt.Sprintf("TestNoise%03d", i), Path: "internal/cli/x_test.go", StartLine: i},
			Confidence: 0.5,
			Reason:     reasonImporter,
		})
	}
	out := formatAffectedResult(result, a)
	if strings.Contains(out, "TestNoise") {
		t.Errorf("importing package's tests must be summarised, not enumerated:\n%s", out[:min(len(out), 600)])
	}
	if !strings.Contains(out, "500 tests") {
		t.Errorf("importing package must report its test COUNT:\n%s", out)
	}
	if len(out) > 2000 {
		t.Errorf("aggregated output should stay small; got %d bytes", len(out))
	}
}
