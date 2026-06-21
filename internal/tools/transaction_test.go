package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// initPlumbWorkspace creates a temp dir with a .plumb/ subdirectory so the
// txlog has a place to write its snapshot directory.
func initPlumbWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".plumb"), 0o755); err != nil {
		t.Fatalf("creating .plumb: %v", err)
	}
	return dir
}

// callTransactionInWorkspace runs transaction_apply with WorkspaceFn wired to
// a real workspace directory so the txlog is exercised.
func callTransactionInWorkspace(t *testing.T, ws string, args map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	deps := WriteDeps{WorkspaceFn: func() string { return ws }}
	return NewTransactionApply(deps).Execute(context.Background(), raw)
}

func callTransaction(t *testing.T, args map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	return NewTransactionApply(WriteDeps{}).Execute(context.Background(), raw)
}

func TestTransaction_TwoFilesSucceed(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	_ = os.WriteFile(a, []byte("hello A"), 0o644)
	_ = os.WriteFile(b, []byte("hello B"), 0o644)

	out, err := callTransaction(t, map[string]any{
		"operations": []map[string]any{
			{"file_path": a, "edits": []map[string]string{{"old_string": "hello", "new_string": "hi"}}},
			{"file_path": b, "edits": []map[string]string{{"old_string": "hello", "new_string": "hi"}}},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "2 files updated") {
		t.Errorf("output: %q", out)
	}
	if got, _ := os.ReadFile(a); string(got) != "hi A" {
		t.Errorf("a: %q", got)
	}
	if got, _ := os.ReadFile(b); string(got) != "hi B" {
		t.Errorf("b: %q", got)
	}
}

func TestTransaction_AllOrNothing_OnValidationFailure(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	_ = os.WriteFile(a, []byte("hello A"), 0o644)
	_ = os.WriteFile(b, []byte("only this"), 0o644)

	// b's edit references a string that doesn't exist. a must NOT be touched.
	_, err := callTransaction(t, map[string]any{
		"operations": []map[string]any{
			{"file_path": a, "edits": []map[string]string{{"old_string": "hello", "new_string": "hi"}}},
			{"file_path": b, "edits": []map[string]string{{"old_string": "missing", "new_string": "hi"}}},
		},
	})
	if err == nil {
		t.Fatal("expected error from validation")
	}
	if got, _ := os.ReadFile(a); string(got) != "hello A" {
		t.Errorf("a should be unchanged, got: %q", got)
	}
	if got, _ := os.ReadFile(b); string(got) != "only this" {
		t.Errorf("b should be unchanged, got: %q", got)
	}
}

func TestTransaction_RejectsDuplicatePath(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	_ = os.WriteFile(a, []byte("hi"), 0o644)

	_, err := callTransaction(t, map[string]any{
		"operations": []map[string]any{
			{"file_path": a, "edits": []map[string]string{{"old_string": "hi", "new_string": "ok"}}},
			{"file_path": a, "edits": []map[string]string{{"old_string": "ok", "new_string": "no"}}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "multiple operations") {
		t.Fatalf("expected duplicate-path rejection, got: %v", err)
	}
}

func TestTransaction_RespectsExpectedSha(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	_ = os.WriteFile(a, []byte("hello"), 0o644)

	sha, err := fileSHA256(a)
	if err != nil {
		t.Fatalf("fileSHA256: %v", err)
	}

	// Correct sha — transaction must succeed.
	_, err = callTransaction(t, map[string]any{
		"operations": []map[string]any{
			{
				"file_path":    a,
				"expected_sha": sha,
				"edits":        []map[string]string{{"old_string": "hello", "new_string": "hi"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error with correct expected_sha: %v", err)
	}

	// Wrong sha — transaction must be rejected.
	_ = os.WriteFile(a, []byte("reset"), 0o644)
	_, err = callTransaction(t, map[string]any{
		"operations": []map[string]any{
			{
				"file_path":    a,
				"expected_sha": "0000000000000000000000000000000000000000000000000000000000000000",
				"edits":        []map[string]string{{"old_string": "reset", "new_string": "done"}},
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "content has changed") {
		t.Fatalf("expected sha rejection, got: %v", err)
	}
}

func TestTransaction_RespectsExpectedMtime(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	_ = os.WriteFile(a, []byte("hello"), 0o644)

	_, err := callTransaction(t, map[string]any{
		"operations": []map[string]any{
			{
				"file_path":      a,
				"expected_mtime": "1999-01-01T00:00:00Z",
				"edits":          []map[string]string{{"old_string": "hello", "new_string": "hi"}},
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "changed since you read it") {
		t.Fatalf("expected mtime rejection, got: %v", err)
	}
}

// TestTransaction_TxlogCommittedOnSuccess verifies that the txlog snapshot
// directory is created during the transaction and removed on Commit, so no
// orphan is left behind after a successful run.
func TestTransaction_TxlogCommittedOnSuccess(t *testing.T) {
	ws := initPlumbWorkspace(t)
	a := filepath.Join(ws, "a.txt")
	b := filepath.Join(ws, "b.txt")
	_ = os.WriteFile(a, []byte("original-a"), 0o644)
	_ = os.WriteFile(b, []byte("original-b"), 0o644)

	out, err := callTransactionInWorkspace(t, ws, map[string]any{
		"operations": []map[string]any{
			{"file_path": a, "edits": []map[string]string{{"old_string": "original-a", "new_string": "new-a"}}},
			{"file_path": b, "edits": []map[string]string{{"old_string": "original-b", "new_string": "new-b"}}},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "2 files updated") {
		t.Errorf("unexpected output: %q", out)
	}

	// No orphaned tx-log directories must remain.
	txLogDir := filepath.Join(ws, ".plumb", "tx-log")
	entries, err := os.ReadDir(txLogDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading tx-log dir: %v", err)
	}
	if len(entries) > 0 {
		t.Errorf("tx-log dir should be empty after success, found: %v", entries)
	}
}

// TestTransaction_TxlogRolledBackOnValidationFailure verifies that when phase 1
// fails (no writes happen), the txlog directory is not created at all — Begin
// is only called at the start of phase 2.
func TestTransaction_TxlogRolledBackOnValidationFailure(t *testing.T) {
	ws := initPlumbWorkspace(t)
	a := filepath.Join(ws, "a.txt")
	_ = os.WriteFile(a, []byte("hello"), 0o644)

	_, err := callTransactionInWorkspace(t, ws, map[string]any{
		"operations": []map[string]any{
			{"file_path": a, "edits": []map[string]string{{"old_string": "missing-string", "new_string": "x"}}},
		},
	})
	if err == nil {
		t.Fatal("expected validation error")
	}

	// Phase 1 failed → phase 2 never started → no txlog directory created.
	txLogDir := filepath.Join(ws, ".plumb", "tx-log")
	entries, _ := os.ReadDir(txLogDir)
	if len(entries) > 0 {
		t.Errorf("no txlog dir expected when phase 1 fails, found: %v", entries)
	}
}

func TestTransaction_DiffGatedByShowWriteDiff(t *testing.T) {
	run := func(showDiff bool) string {
		dir := t.TempDir()
		a := filepath.Join(dir, "a.txt")
		_ = os.WriteFile(a, []byte("hello A\n"), 0o644)
		raw, _ := json.Marshal(map[string]any{
			"operations": []map[string]any{
				{"file_path": a, "edits": []map[string]string{{"old_string": "hello", "new_string": "hi"}}},
			},
		})
		deps := WriteDeps{
			WorkspaceFn:     func() string { return dir },
			ShowWriteDiffFn: func() bool { return showDiff },
		}
		out, err := NewTransactionApply(deps).Execute(context.Background(), raw)
		if err != nil {
			t.Fatalf("Execute (showDiff=%v): %v", showDiff, err)
		}
		return out
	}

	on := run(true)
	if !strings.Contains(on, "--- a/") || !strings.Contains(on, "+++ b/") {
		t.Errorf("show_write_diff on: expected a diff, got:\n%s", on)
	}
	off := run(false)
	if strings.Contains(off, "--- a/") {
		t.Errorf("show_write_diff off: unexpected diff, got:\n%s", off)
	}
}
