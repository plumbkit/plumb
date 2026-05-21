package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/golimpio/plumb/internal/topology"
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
	if !strings.Contains(out, "disabled") {
		t.Errorf("nil result should produce disabled message, got: %s", out)
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
