package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSearchInFiles_ContextLinesAboveOldCap is the F5 fix. The 0–10 ceiling was
// declared ONLY in the JSON schema — nothing server-side ever read it — so it was
// enforced by whichever MCP client validated the schema, and reading behaviour
// around a hit cost several calls. The ceiling is now 50 and enforced by plumb.
func TestSearchInFiles_ContextLinesAboveOldCap(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	for i := range 60 {
		if i == 30 {
			lines = append(lines, "NEEDLE")
			continue
		}
		lines = append(lines, fmt.Sprintf("filler %d", i))
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewSearchInFiles(nil, nil, nil, 0)
	args, _ := json.Marshal(map[string]any{"pattern": "NEEDLE", "path": dir, "context_lines": 25})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("context_lines above the old cap of 10 must be accepted, got: %v", err)
	}
	// 25 lines of context either side must actually be emitted.
	if !strings.Contains(out, "filler 6") || !strings.Contains(out, "filler 54") {
		t.Errorf("expected 25 lines of context either side, got:\n%s", out)
	}
}

// TestSearchInFiles_ContextLinesCapEnforcedServerSide: past the new ceiling the
// call is refused BY PLUMB, not silently honoured and not left to the client.
func TestSearchInFiles_ContextLinesCapEnforcedServerSide(t *testing.T) {
	dir := t.TempDir()
	tool := NewSearchInFiles(nil, nil, nil, 0)
	args, _ := json.Marshal(map[string]any{
		"pattern": "x", "path": dir, "context_lines": searchMaxContextLines + 1,
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), "context_lines") {
		t.Fatalf("expected a server-side context_lines refusal, got: %v", err)
	}
}

// TestSearchInFiles_OutputBudget is the safety half of raising the ceiling:
// search_in_files was the one search tool with no total-output budget, bounded
// only by max_results × (2·context_lines + 1) lines. Truncation must be capped
// AND labelled, never silent.
func TestSearchInFiles_OutputBudget(t *testing.T) {
	dir := t.TempDir()
	// A wide body of matches: 400 files, each with a long matching line.
	long := strings.Repeat("NEEDLE padding padding padding ", 40)
	for i := range 400 {
		p := filepath.Join(dir, fmt.Sprintf("f%03d.txt", i))
		if err := os.WriteFile(p, []byte(long+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tool := NewSearchInFiles(nil, nil, nil, 0)
	args, _ := json.Marshal(map[string]any{
		"pattern": "NEEDLE", "path": dir, "max_results": 2000,
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > searchMaxOutputBytes*2 {
		t.Errorf("output %d bytes exceeds the budget by more than the final summary allows", len(out))
	}
	if !strings.Contains(out, "output truncated") {
		t.Errorf("budget truncation must be labelled, got tail:\n%s", out[max(0, len(out)-400):])
	}
}

// TestSearchInFiles_SmallResultNotLabelledTruncated guards the blast radius: an
// ordinary search must not gain a truncation notice.
func TestSearchInFiles_SmallResultNotLabelledTruncated(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("NEEDLE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewSearchInFiles(nil, nil, nil, 0)
	args, _ := json.Marshal(map[string]any{"pattern": "NEEDLE", "path": dir, "context_lines": 3})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "output truncated") {
		t.Errorf("a small result must not be labelled truncated, got:\n%s", out)
	}
}
