package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func callFileStatus(t *testing.T, tool *FileStatus, paths ...string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"paths": paths})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("file_status Execute: %v", err)
	}
	return out
}

func TestFileStatus_ParseAndValidate(t *testing.T) {
	tool := NewFileStatus(nil)

	if _, err := tool.Execute(context.Background(), json.RawMessage(`{not json`)); err == nil {
		t.Error("expected error for malformed JSON")
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"paths": []}`)); err == nil {
		t.Error("expected error for empty paths")
	}

	tooMany := make([]string, maxFileStatusPaths+1)
	for i := range tooMany {
		tooMany[i] = "f.txt"
	}
	raw, _ := json.Marshal(map[string]any{"paths": tooMany})
	if _, err := tool.Execute(context.Background(), raw); err == nil {
		t.Errorf("expected error for more than %d paths", maxFileStatusPaths)
	}
}

func TestFileStatus_MissingFileReportedNotError(t *testing.T) {
	dir := t.TempDir()
	tool := NewFileStatus(nil)
	out := callFileStatus(t, tool, filepath.Join(dir, "nope.txt"))
	if !strings.Contains(out, "exists: false") {
		t.Errorf("expected exists: false for a missing file, got:\n%s", out)
	}
}

func TestFileStatus_ReportsMtimeAndSize(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(f, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewFileStatus(nil)
	out := callFileStatus(t, tool, f)
	if !strings.Contains(out, "exists: true") {
		t.Errorf("expected exists: true, got:\n%s", out)
	}
	if !strings.Contains(out, "size: 5") {
		t.Errorf("expected size: 5, got:\n%s", out)
	}
	// No WriteTracker → unknown writer.
	if !strings.Contains(out, "last_writer: unknown") {
		t.Errorf("expected last_writer: unknown without a tracker, got:\n%s", out)
	}
}

func TestFileStatus_GitDirty(t *testing.T) {
	dir := initGitRepo(t)
	f := filepath.Join(dir, "tracked.txt")
	if err := os.WriteFile(f, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitExec(t, dir, "add", "tracked.txt")
	gitExec(t, dir, "commit", "-m", "add tracked")

	tool := NewFileStatus(nil)

	clean := callFileStatus(t, tool, f)
	if !strings.Contains(clean, "git_dirty: false") {
		t.Errorf("expected git_dirty: false for a committed file, got:\n%s", clean)
	}

	if err := os.WriteFile(f, []byte("modified"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirty := callFileStatus(t, tool, f)
	if !strings.Contains(dirty, "git_dirty: true") {
		t.Errorf("expected git_dirty: true after modification, got:\n%s", dirty)
	}
}

func TestFileStatus_LastWriterPlumb(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(f, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	writes := NewWriteTracker()
	writes.Record(f) // plumb wrote it this session

	tool := NewFileStatus(writes)
	out := callFileStatus(t, tool, f)
	if !strings.Contains(out, "last_writer: plumb") {
		t.Errorf("expected last_writer: plumb after a tracked write, got:\n%s", out)
	}
	if !strings.Contains(out, "changed_since_plumb_wrote: false") {
		t.Errorf("expected changed_since_plumb_wrote: false for an unchanged file, got:\n%s", out)
	}
}

func TestFileStatus_ChangedSincePlumbWrote(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(f, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	writes := NewWriteTracker()
	writes.Record(f)

	// An external process edits the file after plumb's recorded write, advancing
	// its mtime past the tracked value.
	later := time.Now().Add(2 * time.Second)
	if err := os.WriteFile(f, []byte("v2 external"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(f, later, later); err != nil {
		t.Fatal(err)
	}

	tool := NewFileStatus(writes)
	out := callFileStatus(t, tool, f)
	if !strings.Contains(out, "changed_since_plumb_wrote: true") {
		t.Errorf("expected changed_since_plumb_wrote: true after an external edit, got:\n%s", out)
	}
	if !strings.Contains(out, "last_writer: external") {
		t.Errorf("expected last_writer: external after an external edit, got:\n%s", out)
	}
}

func TestFileStatus_BoundaryViolationReported(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	guard := BoundaryGuard(func(string) error {
		return NewWorkspaceBoundaryError("/some/workspace", f)
	})
	tool := NewFileStatus(nil).WithBoundary(guard)
	out := callFileStatus(t, tool, f)
	if !strings.Contains(out, "error:") {
		t.Errorf("expected an error entry for a boundary violation, got:\n%s", out)
	}
	if strings.Contains(out, "exists: true") {
		t.Errorf("boundary-refused path must not be stat'd, got:\n%s", out)
	}
}

func TestFileStatus_MultiplePaths(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(a, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewFileStatus(nil)
	out := callFileStatus(t, tool, a, b)
	if !strings.Contains(out, "2 path(s)") {
		t.Errorf("expected a 2-path header, got:\n%s", out)
	}
	if !strings.Contains(out, a) || !strings.Contains(out, b) {
		t.Errorf("expected both paths in the output, got:\n%s", out)
	}
}

func TestFileStatus_Metadata(t *testing.T) {
	tool := NewFileStatus(nil)
	if tool.Name() != "file_status" {
		t.Errorf("Name() = %q, want file_status", tool.Name())
	}
	if len(tool.Description()) < 100 {
		t.Error("Description() is suspiciously short")
	}
	var schema map[string]any
	if err := json.Unmarshal(tool.InputSchema(), &schema); err != nil {
		t.Fatalf("InputSchema is not valid JSON: %v", err)
	}
}
