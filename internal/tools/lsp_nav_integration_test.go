//go:build integration

package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
)

// navFixture writes a tiny Go module whose Target function has a blank body line
// (a position no identifier occupies) and a caller, then returns a live gopls
// client plus the file URI. Layout (0-based lines):
//
//	0 package nav
//	1
//	2 func Target() int {
//	3          <- blank line inside the body: "no identifier found"
//	4 	return 41
//	5 }
//	6
//	7 func caller() int { return Target() }
func navFixture(t *testing.T) (lsp.Client, string) {
	t.Helper()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module nav\n\ngo 1.23\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := "package nav\n\n" +
		"func Target() int {\n" +
		"\n" +
		"\treturn 41\n" +
		"}\n\n" +
		"func caller() int { return Target() }\n"
	path := filepath.Join(ws, "nav.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return startGoplsClient(t, ws), protocol.FileURI(path)
}

// runNavUntil retries a nav tool call until it returns without error or the
// deadline passes, tolerating gopls's cold-start package-load latency. With the
// RC3 fix a snapped/by-name query resolves once the package is loaded; without
// it the blank-line call fails "no identifier found" every time and the caller
// eventually times out (the red state).
func runNavUntil(t *testing.T, exec func(context.Context) (string, error), deadline time.Time) (string, error) {
	t.Helper()
	var (
		out     string
		lastErr error
	)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		out, lastErr = exec(ctx)
		cancel()
		if lastErr == nil {
			return out, nil
		}
		if time.Now().After(deadline) {
			return out, lastErr
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func navArgs(t *testing.T, m map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestIntegration_FindReferences_SnapOnBlankLine is the RC3 repro: find_references
// at a blank line inside Target's body fails "no identifier found" against real
// gopls; the snap resolves the enclosing symbol and returns Target's references.
func TestIntegration_FindReferences_SnapOnBlankLine(t *testing.T) {
	client, uri := navFixture(t)
	tool := tools.NewFindReferences(client, nil, time.Minute, 30*time.Second)
	deadline := time.Now().Add(45 * time.Second)

	out, err := runNavUntil(t, func(ctx context.Context) (string, error) {
		return tool.Execute(ctx, navArgs(t, map[string]any{"uri": uri, "line": 3, "character": 0}))
	}, deadline)
	if err != nil {
		t.Fatalf("find_references should snap and succeed, got: %v", err)
	}
	if !strings.Contains(out, "note: no identifier at") {
		t.Errorf("expected a snap note, got:\n%s", out)
	}
	if !strings.Contains(out, "caller") {
		t.Errorf("expected the caller reference after snapping to Target, got:\n%s", out)
	}
}

// TestIntegration_GetDefinition_SnapOnColumnBeyondEOL is the RC3 repro for
// get_definition: gopls returns an empty result (not an error) for a blank
// position, so the observed production failure was "column is beyond end of
// line" — a column past a body line's end. The snap resolves the enclosing
// symbol and returns a definition.
func TestIntegration_GetDefinition_SnapOnColumnBeyondEOL(t *testing.T) {
	client, uri := navFixture(t)
	tool := tools.NewGetDefinition(client, nil, 0, 30*time.Second)
	deadline := time.Now().Add(45 * time.Second)

	// Line 4 is "\treturn 41" (~9 columns); character 80 is well past its end.
	out, err := runNavUntil(t, func(ctx context.Context) (string, error) {
		return tool.Execute(ctx, navArgs(t, map[string]any{"uri": uri, "line": 4, "character": 80}))
	}, deadline)
	if err != nil {
		t.Fatalf("get_definition should snap and succeed, got: %v", err)
	}
	if !strings.Contains(out, "note: no identifier at") {
		t.Errorf("expected a snap note, got:\n%s", out)
	}
	if !strings.Contains(out, "Definition at") {
		t.Errorf("expected a definition after snapping to Target, got:\n%s", out)
	}
}

// TestIntegration_CallHierarchy_SnapOnBlankLine is the RC3 repro for
// call_hierarchy at a non-identifier position (topology unwired).
func TestIntegration_CallHierarchy_SnapOnBlankLine(t *testing.T) {
	client, uri := navFixture(t)
	tool := tools.NewCallHierarchy(client, 30*time.Second)
	deadline := time.Now().Add(45 * time.Second)

	out, err := runNavUntil(t, func(ctx context.Context) (string, error) {
		return tool.Execute(ctx, navArgs(t, map[string]any{"uri": uri, "line": 3, "character": 0, "direction": "incoming"}))
	}, deadline)
	if err != nil {
		t.Fatalf("call_hierarchy should snap and succeed, got: %v", err)
	}
	if !strings.Contains(out, "note: no identifier at") {
		t.Errorf("expected a snap note, got:\n%s", out)
	}
	if !strings.Contains(out, "Target") {
		t.Errorf("expected Target's call hierarchy after snapping, got:\n%s", out)
	}
}

// TestIntegration_CallHierarchy_ByName is the RC3 2a repro: call_hierarchy now
// accepts symbol_name (no line/character), mirroring find_references.
func TestIntegration_CallHierarchy_ByName(t *testing.T) {
	client, uri := navFixture(t)
	tool := tools.NewCallHierarchy(client, 30*time.Second)
	deadline := time.Now().Add(45 * time.Second)

	out, err := runNavUntil(t, func(ctx context.Context) (string, error) {
		return tool.Execute(ctx, navArgs(t, map[string]any{"uri": uri, "symbol_name": "Target", "direction": "incoming"}))
	}, deadline)
	if err != nil {
		t.Fatalf("by-name call_hierarchy should succeed, got: %v", err)
	}
	if !strings.Contains(out, "Call hierarchy for Target") {
		t.Errorf("expected Target's call hierarchy by name, got:\n%s", out)
	}
	if !strings.Contains(out, "caller") {
		t.Errorf("expected caller in the incoming section, got:\n%s", out)
	}
}

// TestIntegration_FindReferences_NoEnclosingActionableError covers the "nothing
// encloses" branch: a query on the blank line between top-level declarations
// (line 6) cannot snap, so the error names nearby symbols and points at
// symbol_name.
func TestIntegration_FindReferences_NoEnclosingActionableError(t *testing.T) {
	client, uri := navFixture(t)
	tool := tools.NewFindReferences(client, nil, time.Minute, 30*time.Second)
	deadline := time.Now().Add(45 * time.Second)

	// Retry only while gopls is still warming (a transient timeout); once loaded
	// the position genuinely has no enclosing symbol and must return the actionable
	// error deterministically.
	var lastErr error
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_, lastErr = tool.Execute(ctx, navArgs(t, map[string]any{"uri": uri, "line": 6, "character": 0}))
		cancel()
		if lastErr != nil && strings.Contains(lastErr.Error(), "did you mean") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected an actionable 'did you mean' error, got: %v", lastErr)
		}
		time.Sleep(300 * time.Millisecond)
	}
	for _, want := range []string{"did you mean", "symbol_name"} {
		if !strings.Contains(lastErr.Error(), want) {
			t.Errorf("actionable error %q missing %q", lastErr.Error(), want)
		}
	}
}
