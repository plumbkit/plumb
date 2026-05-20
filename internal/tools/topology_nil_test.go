package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/golimpio/plumb/internal/topology"
)

func TestTopologyStatus_NilStore(t *testing.T) {
	tool := NewTopologyStatus(func() *topology.Store { return nil }, func() string { return "/ws" })
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "disabled") {
		t.Errorf("expected 'disabled' in output, got: %q", out)
	}
}

func TestTopologySearch_NilStore(t *testing.T) {
	tool := NewTopologySearch(func() *topology.Store { return nil })
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"foo"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "disabled") {
		t.Errorf("expected 'disabled' in output, got: %q", out)
	}
}

func TestTopologyExplore_NilStore(t *testing.T) {
	tool := NewTopologyExplore(func() *topology.Store { return nil })
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"foo"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "disabled") {
		t.Errorf("expected 'disabled' in output, got: %q", out)
	}
}
