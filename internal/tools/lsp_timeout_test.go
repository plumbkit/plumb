package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/tools"
)

// TestWorkspaceSymbols_Timeout verifies that a language server that never
// answers makes the tool fail fast with an actionable message instead of
// blocking until the MCP client's own timeout fires.
func TestWorkspaceSymbols_Timeout(t *testing.T) {
	mock := &mockLSP{block: true}
	tool := tools.NewWorkspaceSymbols(mock, nil, 0, 20*time.Millisecond, nil)
	args, _ := json.Marshal(map[string]any{"query": "Foo"})

	start := time.Now()
	_, err := tool.Execute(context.Background(), args)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error from a blocked language server")
	}
	if elapsed > time.Second {
		t.Errorf("expected prompt failure near the 20ms deadline, took %v", elapsed)
	}
	if !strings.Contains(err.Error(), "did not respond within") {
		t.Errorf("expected the friendly timeout message, got: %v", err)
	}
}

// TestFileOutline_Timeout exercises the same path through a DocumentSymbols
// query to confirm the deadline is applied uniformly across query tools.
func TestFileOutline_Timeout(t *testing.T) {
	mock := &mockLSP{block: true}
	tool := tools.NewFileOutline(mock, nil, 0, 20*time.Millisecond)
	_, uri := writeOutlineFile(t)
	args, _ := json.Marshal(map[string]any{"uri": uri})

	start := time.Now()
	_, err := tool.Execute(context.Background(), args)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error from a blocked language server")
	}
	if elapsed > time.Second {
		t.Errorf("expected prompt failure near the 20ms deadline, took %v", elapsed)
	}
	if !strings.Contains(err.Error(), "did not respond within") {
		t.Errorf("expected the friendly timeout message, got: %v", err)
	}
}
