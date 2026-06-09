package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
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

func TestTopologyAmbiguityNote(t *testing.T) {
	if note := topologyAmbiguityNote("foo", nil); note != "" {
		t.Errorf("unambiguous name should produce no note, got: %q", note)
	}
	alts := []topology.Node{
		{Kind: topology.KindFunction, Name: "foo", Path: "b/bar.go", StartLine: 12},
	}
	note := topologyAmbiguityNote("foo", alts)
	if !strings.Contains(note, "matched 2 symbols") {
		t.Errorf("note should report the total match count, got: %q", note)
	}
	if !strings.Contains(note, "b/bar.go") || !strings.Contains(note, "L12") {
		t.Errorf("note should list the alternative's path and line, got: %q", note)
	}
}

func TestTopologyImpact_FormatNilResult(t *testing.T) {
	a := topologyImpactArgs{Name: "foo", Depth: 3, EdgeKinds: []string{"calls"}}
	out := formatImpactResult(nil, a, nil)
	if strings.Contains(out, "disabled") {
		t.Errorf("nil result is a not-found case, not 'disabled'; got: %s", out)
	}
	if !strings.Contains(out, "not found in the index") {
		t.Errorf("expected not-found message, got: %s", out)
	}
}
