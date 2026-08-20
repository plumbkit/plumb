package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
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

// TestSearchInFiles_OutputBudgetIsHardForOneLongLine is the regression test for
// a defect an independent review found: the budget was checked only BEFORE each
// line write, so a single long match (searchMaxLineBytes allows 1 MiB) sailed
// past every check and carried total output to ~5x the documented 200 KiB cap.
// The cap is documented in the schema as absolute, so it must actually be one.
func TestSearchInFiles_OutputBudgetIsHardForOneLongLine(t *testing.T) {
	dir := t.TempDir()
	// One matching line of ~900 KiB — under the per-line skip threshold, so it is
	// scanned and emitted rather than skipped.
	long := "NEEDLE" + strings.Repeat("x", 900*1024)
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(long+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewSearchInFiles(nil, nil, nil, 0)
	args, _ := json.Marshal(map[string]any{"pattern": "NEEDLE", "path": dir})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	// Allow the summary and path note past the budget, but nothing like a 900 KiB
	// overshoot.
	if len(out) > searchMaxOutputBytes+8*1024 {
		t.Errorf("output was %d bytes against a %d-byte cap — the budget is not hard",
			len(out), searchMaxOutputBytes)
	}
	if !strings.Contains(out, "output truncated") {
		t.Errorf("truncation must be labelled, got tail:\n%s", out[max(0, len(out)-300):])
	}
}

// TestSearchInFiles_TruncatedLineStaysValidUTF8 guards the budget fix's own
// blast radius: cutting a long line to fit the byte budget must not split a
// rune. A raw byte slice does exactly that on any non-ASCII line, emitting an
// invalid sequence into the response.
func TestSearchInFiles_TruncatedLineStaysValidUTF8(t *testing.T) {
	dir := t.TempDir()
	// Multi-byte runes throughout, so a byte-offset cut almost certainly lands
	// mid-rune.
	long := "NEEDLE" + strings.Repeat("éü→", 100*1024)
	if err := os.WriteFile(filepath.Join(dir, "utf8.txt"), []byte(long+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewSearchInFiles(nil, nil, nil, 0)
	args, _ := json.Marshal(map[string]any{"pattern": "NEEDLE", "path": dir})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(out) {
		t.Error("truncating a long line produced invalid UTF-8")
	}
	if !strings.Contains(out, "output truncated") {
		t.Errorf("expected the truncation label, got tail:\n%s", out[max(0, len(out)-200):])
	}
}

func TestRuneSafeCut(t *testing.T) {
	s := "aéb" // 1 + 2 + 1 bytes
	cases := []struct{ n, want int }{
		{0, 0},
		{1, 1}, // after 'a'
		{2, 1}, // mid-'é' → back off to 1
		{3, 3}, // after 'é'
		{4, 4}, // whole string
		{99, 4},
	}
	for _, tc := range cases {
		if got := runeSafeCut(s, tc.n); got != tc.want {
			t.Errorf("runeSafeCut(%q, %d) = %d, want %d", s, tc.n, got, tc.want)
		}
		if !utf8.ValidString(s[:runeSafeCut(s, tc.n)]) {
			t.Errorf("runeSafeCut(%q, %d) produced invalid UTF-8", s, tc.n)
		}
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
