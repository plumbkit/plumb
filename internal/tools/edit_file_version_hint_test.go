package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestEditFile_VersionGuardHint_OnlyForAnchoredBatch proves the reconcile escape
// hatch is offered only when every edit is anchor-based. A range-mode edit has no
// exact-once anchor, so reconciling it against changed content could land on
// shifted lines — the hint must not tempt the agent toward that.
func TestEditFile_VersionGuardHint_OnlyForAnchoredBatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.go")
	_ = os.WriteFile(path, []byte("a\nb\nc\n"), 0o644)
	staleMtime := time.Now().Add(-time.Hour).Format(time.RFC3339Nano)

	_, err := callEditFile(t, map[string]any{
		"file_path":      path,
		"expected_mtime": staleMtime,
		"edits":          []map[string]any{{"start_line": 2, "new_string": "B"}},
	})
	if err == nil || !strings.Contains(err.Error(), "modified since you read it") {
		t.Fatalf("expected mtime-mismatch rejection, got: %v", err)
	}
	if strings.Contains(err.Error(), "reconcile") {
		t.Errorf("a range-mode (anchorless) batch must not be offered the reconcile hint, got: %v", err)
	}

	// The rejected edit must not have touched the file.
	if data, _ := os.ReadFile(path); string(data) != "a\nb\nc\n" {
		t.Errorf("rejected edit modified the file: %q", data)
	}
}
