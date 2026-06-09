package tools

import (
	"context"
	"encoding/json"
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

func TestTopologyAffected_FormatSurfacesConfidenceAndRecall(t *testing.T) {
	a := topologyAffectedArgs{Symbols: []string{"Foo"}, MaxResults: 50}
	result := &affectedResult{
		Tests: []affectedTest{
			{Node: topology.Node{Name: "TestFoo", Path: "foo_test.go", StartLine: 10}, Confidence: 0.8, Reason: "dependency edge"},
			{Node: topology.Node{Name: "TestBar", Path: "bar_test.go", StartLine: 5}, Confidence: 0.5, Reason: "co-located"},
		},
	}
	out := formatAffectedResult(result, a)
	for _, want := range []string{"TestFoo", "dependency edge", "co-located", "biased toward recall", "confidence 0.8", "confidence 0.5"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}
