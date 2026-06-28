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

// TestCheckExpectedVersion_HintSuppressedInStrict proves the reconcile hint is
// shown only when it would actually help: in strict mode reconcile alone does not
// satisfy checkStrictRead (a fresh read is still required), so the hint is omitted
// to avoid a wasted round-trip.
func TestCheckExpectedVersion_HintSuppressedInStrict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.go")
	_ = os.WriteFile(path, []byte("hello\n"), 0o644)
	a := editFileArgs{
		Path:          path,
		ExpectedMtime: time.Now().Add(-time.Hour).Format(time.RFC3339Nano),
		Edits:         []strEdit{{OldStr: "hello", NewStr: "world"}},
	}

	err := checkExpectedVersion(path, a, false)
	if err == nil || !strings.Contains(err.Error(), "reconcile: true") {
		t.Fatalf("non-strict should include the reconcile hint, got: %v", err)
	}

	err = checkExpectedVersion(path, a, true)
	if err == nil || strings.Contains(err.Error(), "reconcile: true") {
		t.Fatalf("strict mode should suppress the reconcile hint, got: %v", err)
	}
	if !strings.Contains(err.Error(), "modified since you read it") {
		t.Errorf("strict mode should still reject with the mismatch message, got: %v", err)
	}
}
