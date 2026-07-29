package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/tools"
)

// Warming-vs-ready split for the hard-fail LSP tools: when the warm-up probe
// reports the server is still completing its handshake, the failure must be
// rewritten into a still-warming advisory that names the tree-sitter/topology
// backup — and the 0-based coordinate hint (misleading there) suppressed. On a
// ready server the ordinary error flavour is kept.

// stubWarmup returns an LSPWarmupFn probe fixed to one state.
func stubWarmup(warming bool, elapsed time.Duration) tools.LSPWarmupFn {
	return func(string) (bool, time.Duration) { return warming, elapsed }
}

// coldMock fails every LSP method, as a server mid-handshake does.
func coldMock() *mockLSP { return &mockLSP{err: errors.New("connection is closed")} }

func assertWarmingAdvisory(t *testing.T, err error, tool string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected the cold-LSP failure to surface as an error", tool)
	}
	msg := err.Error()
	if !strings.Contains(msg, "still warming") || !strings.Contains(msg, "tree-sitter") || !strings.Contains(msg, "daemon_info") {
		t.Errorf("%s: expected the still-warming advisory naming the topology backup, got: %v", tool, err)
	}
	if strings.Contains(msg, "0-based") {
		t.Errorf("%s: the 0-based coordinate hint is misleading on a cold server, got: %v", tool, err)
	}
}

func TestExplainSymbol_ColdServer_WarmingSuppressesCoordinateHint(t *testing.T) {
	tool := tools.NewExplainSymbol(coldMock(), nil, 0, 0).WithLSPWarmup(stubWarmup(true, 4*time.Second))
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/main.go", "line": 2, "character": 5})
	_, err := tool.Execute(context.Background(), args)
	assertWarmingAdvisory(t, err, "explain_symbol")
}

func TestExplainSymbol_ReadyServer_KeepsCoordinateHint(t *testing.T) {
	tool := tools.NewExplainSymbol(coldMock(), nil, 0, 0).WithLSPWarmup(stubWarmup(false, 0))
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/main.go", "line": 2, "character": 5})
	_, err := tool.Execute(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), "0-based") {
		t.Fatalf("a ready server's genuine position error keeps the 0-based hint, got: %v", err)
	}
}

func TestTypeHierarchy_ColdServer_WarmingSuppressesCoordinateHint(t *testing.T) {
	tool := tools.NewTypeHierarchy(coldMock(), 0).WithLSPWarmup(stubWarmup(true, 2*time.Second))
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/main.go", "line": 2, "character": 5})
	_, err := tool.Execute(context.Background(), args)
	assertWarmingAdvisory(t, err, "type_hierarchy")
}

func TestFindReferences_ByName_ColdServer_WarmingAdvisory(t *testing.T) {
	tool := tools.NewFindReferences(coldMock(), nil, 0, 0).WithLSPWarmup(stubWarmup(true, 3*time.Second))
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/main.go", "symbol_name": "Foo"})
	_, err := tool.Execute(context.Background(), args)
	assertWarmingAdvisory(t, err, "find_references")
}

func TestFindReferences_ByPosition_ColdServer_WarmingSuppressesSnapAndHint(t *testing.T) {
	tool := tools.NewFindReferences(coldMock(), nil, 0, 0).WithLSPWarmup(stubWarmup(true, 3*time.Second))
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/main.go", "line": 2, "character": 5})
	_, err := tool.Execute(context.Background(), args)
	assertWarmingAdvisory(t, err, "find_references")
}

func TestCallHierarchy_ByName_ColdServer_WarmingAdvisory(t *testing.T) {
	tool := tools.NewCallHierarchy(coldMock(), 0).WithLSPWarmup(stubWarmup(true, 3*time.Second))
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/main.go", "symbol_name": "Foo"})
	_, err := tool.Execute(context.Background(), args)
	assertWarmingAdvisory(t, err, "call_hierarchy")
}

func TestSafeDeleteSymbol_ColdServer_WarmingAdvisory(t *testing.T) {
	tool := tools.NewSafeDeleteSymbol(coldMock(), 0).WithLSPWarmup(stubWarmup(true, 3*time.Second))
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/main.go", "name_path": "Foo"})
	_, err := tool.Execute(context.Background(), args)
	assertWarmingAdvisory(t, err, "safe_delete_symbol")
}

// rename_symbol keeps its own failure-hint builder (no topology fallback by
// design); when the probe says warming, the guidance must say so and point at
// daemon_info.
func TestRenameSymbol_ColdServer_HintMentionsWarming(t *testing.T) {
	tool := tools.NewRenameSymbol(coldMock(), 0).WithLSPWarmup(stubWarmup(true, 3*time.Second))
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/main.go", "line": 2, "character": 5, "new_name": "Bar"})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected the cold-LSP rename failure to surface as an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "still warming") || !strings.Contains(msg, "daemon_info") {
		t.Errorf("rename guidance must mention the warming state and daemon_info, got: %v", err)
	}
}
