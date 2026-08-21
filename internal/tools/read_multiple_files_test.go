package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// read_multiple_files calls read_file in-process, bypassing the MCP dispatch
// alias layer — so it must hand read_file its canonical "file_path" key, not
// the pre-0.7.19 "path". This guards that contract: a key drift would make
// every file report "file_path is required".
func TestReadMultipleFiles_ReadsContentAndReportsPerFileErrors(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.txt")
	fileB := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(fileA, []byte("alpha-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("bravo-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.txt")

	raw, _ := json.Marshal(map[string]any{"paths": []string{fileA, missing, fileB}})
	out, err := (&ReadMultipleFiles{}).Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if strings.Contains(out, "file_path is required") {
		t.Fatalf("inner read used the wrong key for read_file:\n%s", out)
	}
	if !strings.Contains(out, "alpha-content") || !strings.Contains(out, "bravo-content") {
		t.Fatalf("expected both file contents in output:\n%s", out)
	}
	// The missing file errors inline without blocking the readable ones.
	if !strings.Contains(out, "### ERROR:") {
		t.Fatalf("expected an inline error for the missing path:\n%s", out)
	}
}

// mtimeForPath pulls the mtime read_multiple_files recorded for path's own
// per-file block out of a batch-read response.
func mtimeForPath(t *testing.T, out, path string) string {
	t.Helper()
	idx := strings.Index(out, "### "+path)
	if idx < 0 {
		t.Fatalf("output has no block for %s:\n%s", path, out)
	}
	re := regexp.MustCompile(`mtime=(\S+)`)
	m := re.FindStringSubmatch(out[idx:])
	if m == nil {
		t.Fatalf("no mtime found for %s in:\n%s", path, out)
	}
	return m[1]
}

// PLAN-357 acceptance: prior to this fix, read_multiple_files built its inner
// reader as a bare &ReadFile{} with no tracker, so ReadTracker.Record was
// never called (Record is nil-safe and silently no-ops). Under [edits] strict
// a subsequent edit_file always failed with "has not been read in this daemon
// session", even though the file had just been read in this same batch call.
func TestReadMultipleFiles_StrictMode_BatchReadThenEditSucceeds(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.txt")
	pathB := filepath.Join(dir, "b.txt")
	pathC := filepath.Join(dir, "c.txt")
	for _, p := range []string{pathA, pathB, pathC} {
		if err := os.WriteFile(p, []byte("hello\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tracker := NewReadTracker()
	out, err := NewReadMultipleFiles(tracker).Execute(context.Background(),
		mustJSON(map[string]any{"paths": []string{pathA, pathB, pathC}}))
	if err != nil {
		t.Fatalf("batch read failed: %v", err)
	}

	mtime := mtimeForPath(t, out, pathB)

	editOut, err := NewEditFile(WriteDeps{Reads: tracker, Strict: func() bool { return true }}).
		Execute(context.Background(), mustJSON(map[string]any{
			"file_path":      pathB,
			"expected_mtime": mtime,
			"edits":          []map[string]string{{"old_string": "hello", "new_string": "world"}},
		}))
	if err != nil {
		t.Fatalf("strict-mode edit_file after a batch read must succeed, got: %v", err)
	}
	if !strings.Contains(editOut, "applied 1") {
		t.Fatalf("unexpected edit_file output: %q", editOut)
	}
}

// The native-edit-lane hint is a per-CALL orientation nudge, not a per-file
// safety signal (unlike the peer-write warning or the outside-workspace
// label) — a 5-file batch must carry it once, not five times, and the
// per-file read_file hint line must not leak through the inner reader.
func TestReadMultipleFiles_EditLaneHint_ConsolidatedOnce(t *testing.T) {
	dir := t.TempDir()
	paths := make([]string, 0, 5)
	for i := range 5 {
		p := filepath.Join(dir, fmt.Sprintf("f%d.txt", i))
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}

	out, err := NewReadMultipleFiles(NewReadTracker()).
		WithClient(func() string { return "claude-code" }).
		Execute(context.Background(), mustJSON(map[string]any{"paths": paths}))
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if n := strings.Count(out, "To edit any of these files"); n != 1 {
		t.Fatalf("expected exactly 1 consolidated edit-lane hint for a 5-file batch, got %d:\n%s", n, out)
	}
	if strings.Contains(out, "To edit: use edit_file (not the native Edit tool) with expected_mtime=") {
		t.Fatalf("per-file native-edit hint leaked through — it must be suppressed on the inner reader:\n%s", out)
	}
}

// blockFor extracts one path's '### path' ... section from a batch-read
// response, up to (but not including) the next '### ' heading or EOF.
func blockFor(t *testing.T, out, path string) string {
	t.Helper()
	idx := strings.Index(out, "### "+path)
	if idx < 0 {
		t.Fatalf("output has no block for %s:\n%s", path, out)
	}
	end := len(out)
	if next := strings.Index(out[idx+len("### "+path):], "\n### "); next >= 0 {
		end = idx + len("### "+path) + next
	}
	return out[idx:end]
}

// PLAN-357 commit 2: start_line/end_line apply uniformly to EVERY path in the
// call — no per-path override.
func TestReadMultipleFiles_UniformSlicing_StartEndLine(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.txt")
	pathB := filepath.Join(dir, "b.txt")
	content := "line1\nline2\nline3\nline4\nline5\n"
	for _, p := range []string{pathA, pathB} {
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	out, err := (&ReadMultipleFiles{}).Execute(context.Background(), mustJSON(map[string]any{
		"paths":      []string{pathA, pathB},
		"start_line": 2,
		"end_line":   3,
	}))
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	for _, p := range []string{pathA, pathB} {
		block := blockFor(t, out, p)
		if !strings.Contains(block, "line2") || !strings.Contains(block, "line3") {
			t.Fatalf("expected line2/line3 in %s's slice:\n%s", p, block)
		}
		if strings.Contains(block, "line1") || strings.Contains(block, "line4") || strings.Contains(block, "line5") {
			t.Fatalf("start_line/end_line should have restricted %s to lines 2-3 only:\n%s", p, block)
		}
	}
}

// PLAN-357 commit 2: pattern applies uniformly to EVERY path in the call.
func TestReadMultipleFiles_UniformSlicing_Pattern(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.txt")
	pathB := filepath.Join(dir, "b.txt")
	content := "alpha\nneedle-here\nomega\n"
	for _, p := range []string{pathA, pathB} {
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	out, err := (&ReadMultipleFiles{}).Execute(context.Background(), mustJSON(map[string]any{
		"paths":   []string{pathA, pathB},
		"pattern": "needle",
	}))
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	for _, p := range []string{pathA, pathB} {
		block := blockFor(t, out, p)
		if !strings.Contains(block, "needle-here") {
			t.Fatalf("expected a pattern match for %s:\n%s", p, block)
		}
		if strings.Contains(block, "alpha") || strings.Contains(block, "omega") {
			t.Fatalf("search mode should return only matching lines for %s:\n%s", p, block)
		}
	}
}
