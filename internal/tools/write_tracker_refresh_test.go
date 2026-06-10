package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A successful write must refresh the ReadTracker, so the session's own
// subsequent edit is treated as operating on known-current content. This is
// the load-bearing behaviour behind both strict-mode chaining (no re-read) and
// the changedSinceSessionRead staleness guard not false-positiving on a
// session's own consecutive writes.

func TestEditFile_StrictMode_ChainedEditsNoReread(t *testing.T) {
	t.Setenv("PLUMB_STRICT_EDITS", "1")
	path := filepath.Join(t.TempDir(), "f.go")
	_ = os.WriteFile(path, []byte("a\nb\nc\n"), 0o644)

	tracker := NewReadTracker()
	deps := WriteDeps{Reads: tracker, Writes: NewWriteTracker(), Strict: func() bool { return true }}

	// One read satisfies strict mode for the first edit.
	_, _ = NewReadFile(tracker).Execute(context.Background(), mustJSON(map[string]any{"file_path": path}))
	if _, err := NewEditFile(deps).Execute(context.Background(), mustJSON(map[string]any{
		"file_path": path,
		"edits":     []map[string]string{{"old_string": "a", "new_string": "A"}},
	})); err != nil {
		t.Fatalf("first edit failed: %v", err)
	}

	// The first edit bumped the mtime. Without the tracker refresh this second
	// edit would be rejected by strict mode ("changed since you read it"). With
	// it, the session's own write counts as the current read.
	out, err := NewEditFile(deps).Execute(context.Background(), mustJSON(map[string]any{
		"file_path": path,
		"edits":     []map[string]string{{"old_string": "b", "new_string": "B"}},
	}))
	if err != nil {
		t.Fatalf("chained edit should not require a re-read after the session's own edit: %v", err)
	}
	if !strings.Contains(out, "applied 1") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestEditFile_OwnConsecutiveEdits_NoStaleWarning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.go")
	_ = os.WriteFile(path, []byte("a\nb\nc\n"), 0o644)

	tracker := NewReadTracker()
	deps := WriteDeps{Reads: tracker, Writes: NewWriteTracker()}

	_, _ = NewReadFile(tracker).Execute(context.Background(), mustJSON(map[string]any{"file_path": path}))
	if _, err := NewEditFile(deps).Execute(context.Background(), mustJSON(map[string]any{
		"file_path": path,
		"edits":     []map[string]string{{"old_string": "a", "new_string": "A"}},
	})); err != nil {
		t.Fatalf("first edit failed: %v", err)
	}
	out, err := NewEditFile(deps).Execute(context.Background(), mustJSON(map[string]any{
		"file_path": path,
		"edits":     []map[string]string{{"old_string": "b", "new_string": "B"}},
	}))
	if err != nil {
		t.Fatalf("second edit failed: %v", err)
	}
	if strings.Contains(out, "plumb-warn") {
		t.Fatalf("a session's own consecutive edit must not warn about external change:\n%s", out)
	}
}

func TestEditFile_ExternalChange_StillWarns(t *testing.T) {
	// The refresh must NOT mask a genuine external edit between two of our edits.
	path := filepath.Join(t.TempDir(), "f.go")
	_ = os.WriteFile(path, []byte("a\nb\nc\n"), 0o644)

	tracker := NewReadTracker()
	deps := WriteDeps{Reads: tracker, Writes: NewWriteTracker()}

	_, _ = NewReadFile(tracker).Execute(context.Background(), mustJSON(map[string]any{"file_path": path}))
	if _, err := NewEditFile(deps).Execute(context.Background(), mustJSON(map[string]any{
		"file_path": path,
		"edits":     []map[string]string{{"old_string": "a", "new_string": "A"}},
	})); err != nil {
		t.Fatalf("first edit failed: %v", err)
	}

	// Simulate a peer/human editing the file after our write, advancing mtime
	// past what recordWritten recorded.
	if err := os.WriteFile(path, []byte("A\nb\nPEER\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	out, err := NewEditFile(deps).Execute(context.Background(), mustJSON(map[string]any{
		"file_path": path,
		"edits":     []map[string]string{{"old_string": "b", "new_string": "B"}},
	}))
	if err != nil {
		t.Fatalf("edit failed: %v", err)
	}
	if !strings.Contains(out, "plumb-warn") {
		t.Fatalf("a genuine external change between our edits must still warn:\n%s", out)
	}
}

func TestWriteFile_RefreshesReadTracker_NoOverwriteRefusal(t *testing.T) {
	// write_file's own prior write must satisfy changedSinceSessionRead, so a
	// session that reads then write_files twice is never refused on its own work.
	path := filepath.Join(t.TempDir(), "f.go")
	_ = os.WriteFile(path, []byte("v0\n"), 0o644)

	tracker := NewReadTracker()
	deps := WriteDeps{Reads: tracker, Writes: NewWriteTracker()}

	_, _ = NewReadFile(tracker).Execute(context.Background(), mustJSON(map[string]any{"file_path": path}))
	if _, err := NewWriteFile(deps).Execute(context.Background(), mustJSON(map[string]any{
		"file_path": path, "content": "v1\n",
	})); err != nil {
		t.Fatalf("first write failed: %v", err)
	}
	// Without the tracker refresh this second write would be refused
	// ("changed on disk since you read it") because the first write bumped mtime.
	if _, err := NewWriteFile(deps).Execute(context.Background(), mustJSON(map[string]any{
		"file_path": path, "content": "v2\n",
	})); err != nil {
		t.Fatalf("second write should not be refused after the session's own write: %v", err)
	}
}
