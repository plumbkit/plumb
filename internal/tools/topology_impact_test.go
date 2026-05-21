package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/golimpio/plumb/internal/topology"
)

func TestTopologyImpact_NilStore(t *testing.T) {
	tool := NewTopologyImpact(func() *topology.Store { return nil })
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"foo"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "disabled") {
		t.Errorf("expected 'disabled' message, got: %s", out)
	}
}

func TestTopologyImpact_MissingName(t *testing.T) {
	tool := NewTopologyImpact(func() *topology.Store { return nil })
	_, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Error("expected error for missing name")
	}
}

func TestTopologyImpact_Defaults(t *testing.T) {
	a, err := parseTopologyImpactArgs(json.RawMessage(`{"name":"foo"}`))
	if err != nil {
		t.Fatal(err)
	}
	if a.Depth != 3 {
		t.Errorf("depth default=%d, want 3", a.Depth)
	}
	if a.MaxNodes != 100 {
		t.Errorf("max_nodes default=%d, want 100", a.MaxNodes)
	}
	if len(a.EdgeKinds) == 0 {
		t.Error("expected default edge_kinds")
	}
}

func TestTopologyImpact_FormatNilResult(t *testing.T) {
	a := topologyImpactArgs{Name: "foo", Depth: 3, EdgeKinds: []string{"calls"}}
	out := formatImpactResult(nil, a)
	if !strings.Contains(out, "disabled") {
		t.Errorf("nil result should produce disabled message, got: %s", out)
	}
}
