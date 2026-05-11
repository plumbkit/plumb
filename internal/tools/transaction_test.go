package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func callTransaction(t *testing.T, args map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	return NewTransactionApply(nil, nil, nil, nil).Execute(context.Background(), raw)
}

func TestTransaction_TwoFilesSucceed(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	_ = os.WriteFile(a, []byte("hello A"), 0o644)
	_ = os.WriteFile(b, []byte("hello B"), 0o644)

	out, err := callTransaction(t, map[string]any{
		"operations": []map[string]any{
			{"path": a, "edits": []map[string]string{{"old_str": "hello", "new_str": "hi"}}},
			{"path": b, "edits": []map[string]string{{"old_str": "hello", "new_str": "hi"}}},
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
			{"path": a, "edits": []map[string]string{{"old_str": "hello", "new_str": "hi"}}},
			{"path": b, "edits": []map[string]string{{"old_str": "missing", "new_str": "hi"}}},
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
			{"path": a, "edits": []map[string]string{{"old_str": "hi", "new_str": "ok"}}},
			{"path": a, "edits": []map[string]string{{"old_str": "ok", "new_str": "no"}}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "multiple operations") {
		t.Fatalf("expected duplicate-path rejection, got: %v", err)
	}
}

func TestTransaction_RespectsExpectedMtime(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	_ = os.WriteFile(a, []byte("hello"), 0o644)

	_, err := callTransaction(t, map[string]any{
		"operations": []map[string]any{
			{
				"path":           a,
				"expected_mtime": "1999-01-01T00:00:00Z",
				"edits":          []map[string]string{{"old_str": "hello", "new_str": "hi"}},
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "changed since you read it") {
		t.Fatalf("expected mtime rejection, got: %v", err)
	}
}
