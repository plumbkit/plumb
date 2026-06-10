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

func TestEditFile_StrictMode_RequiresRead(t *testing.T) {
	t.Setenv("PLUMB_STRICT_EDITS", "1")
	path := filepath.Join(t.TempDir(), "f.go")
	_ = os.WriteFile(path, []byte("hello\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"file_path": path,
		"edits":     []map[string]string{{"old_string": "hello", "new_string": "world"}},
	})
	if err == nil || !strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("expected strict-mode read-required error, got: %v", err)
	}
}

func TestEditFile_StrictMode_AfterRead(t *testing.T) {
	t.Setenv("PLUMB_STRICT_EDITS", "1")
	path := filepath.Join(t.TempDir(), "f.go")
	_ = os.WriteFile(path, []byte("hello\n"), 0o644)

	// Read and edit share one tracker; strict mode picks up the recorded mtime.
	tracker := NewReadTracker()
	_, _ = NewReadFile(tracker).Execute(context.Background(), mustJSON(map[string]any{"file_path": path}))
	out, err := NewEditFile(WriteDeps{Reads: tracker, Strict: func() bool { return true }}).
		Execute(context.Background(), mustJSON(map[string]any{
			"file_path": path,
			"edits":     []map[string]string{{"old_string": "hello", "new_string": "world"}},
		}))
	if err != nil {
		t.Fatalf("expected success after read, got: %v", err)
	}
	if !strings.Contains(out, "applied 1") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestEditFile_StrictMode_TrackerIsolation(t *testing.T) {
	// Two trackers (simulating two sessions). A read in tracker1 must not
	// satisfy strict mode for an edit using tracker2 — proves per-session
	// isolation that the 0.5.2 process-global map lacked.
	path := filepath.Join(t.TempDir(), "f.go")
	_ = os.WriteFile(path, []byte("hello\n"), 0o644)

	sessionA := NewReadTracker()
	sessionB := NewReadTracker()
	_, _ = NewReadFile(sessionA).Execute(context.Background(), mustJSON(map[string]any{"file_path": path}))

	_, err := NewEditFile(WriteDeps{Reads: sessionB, Strict: func() bool { return true }}).
		Execute(context.Background(), mustJSON(map[string]any{
			"file_path": path,
			"edits":     []map[string]string{{"old_string": "hello", "new_string": "world"}},
		}))
	if err == nil || !strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("session B's edit should have been rejected (session A read), got: %v", err)
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func callEditFile(t *testing.T, args map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	return NewEditFile(WriteDeps{Reads: NewReadTracker()}).Execute(context.Background(), raw)
}

func TestEditFile_ReplaceAll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.go")
	_ = os.WriteFile(path, []byte("x := old\ny := old\nz := old\n"), 0o644)

	// Without replace_all, three occurrences are ambiguous and rejected.
	_, err := callEditFile(t, map[string]any{
		"file_path": path,
		"edits":     []map[string]any{{"old_string": "old", "new_string": "new"}},
	})
	if err == nil || !strings.Contains(err.Error(), "appears") && !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguous-match error without replace_all, got: %v", err)
	}

	// With replace_all, all three are replaced in one edit.
	out, err := callEditFile(t, map[string]any{
		"file_path": path,
		"edits":     []map[string]any{{"old_string": "old", "new_string": "new", "replace_all": true}},
	})
	if err != nil {
		t.Fatalf("replace_all edit failed: %v", err)
	}
	if !strings.Contains(out, "applied 1 edit") {
		t.Errorf("unexpected output: %q", out)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "old") || strings.Count(string(data), "new") != 3 {
		t.Errorf("replace_all should replace every occurrence, got: %q", data)
	}
}

func TestEditFile_RecoverStringEncodedEdits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.go")
	_ = os.WriteFile(path, []byte("hello\n"), 0o644)

	// Some clients double-encode the edits array as a JSON string; it must still
	// apply rather than failing with an opaque unmarshal error.
	out, err := callEditFile(t, map[string]any{
		"file_path": path,
		"edits":     `[{"old_string":"hello","new_string":"world"}]`,
	})
	if err != nil {
		t.Fatalf("string-encoded edits should be recovered, got: %v", err)
	}
	if !strings.Contains(out, "applied 1 edit") {
		t.Errorf("unexpected output: %q", out)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), "world") {
		t.Errorf("edit not applied: %q", data)
	}
}

func TestEditFile_ReconcileBypassesMtimeGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.go")
	_ = os.WriteFile(path, []byte("hello\n"), 0o644)
	staleMtime := time.Now().Add(-time.Hour).Format(time.RFC3339Nano)

	// A stale expected_mtime is normally rejected.
	_, err := callEditFile(t, map[string]any{
		"file_path":      path,
		"expected_mtime": staleMtime,
		"edits":          []map[string]any{{"old_string": "hello", "new_string": "world"}},
	})
	if err == nil || !strings.Contains(err.Error(), "modified since you read it") {
		t.Fatalf("expected mtime-mismatch rejection, got: %v", err)
	}

	// With reconcile, the stale mtime is ignored and the unique anchor still applies.
	out, err := callEditFile(t, map[string]any{
		"file_path":      path,
		"expected_mtime": staleMtime,
		"reconcile":      true,
		"edits":          []map[string]any{{"old_string": "hello", "new_string": "world"}},
	})
	if err != nil {
		t.Fatalf("reconcile should bypass the stale mtime, got: %v", err)
	}
	if !strings.Contains(out, "applied 1 edit") {
		t.Errorf("unexpected output: %q", out)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), "world") {
		t.Errorf("edit not applied: %q", data)
	}
}

func TestEditFile_BasicReplace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.go")
	_ = os.WriteFile(path, []byte("package main\n\nfunc Hello() {}\n"), 0o644)

	out, err := callEditFile(t, map[string]any{
		"file_path": path,
		"edits": []map[string]string{
			{"old_string": "func Hello() {}", "new_string": "func Hello() { return }"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "applied 1 edit") {
		t.Errorf("unexpected output: %q", out)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "func Hello() { return }") {
		t.Errorf("edit not applied: %q", data)
	}
}

func TestEditFile_MultipleEdits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("foo\nbar\nbaz\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"file_path": path,
		"edits": []map[string]string{
			{"old_string": "foo", "new_string": "FOO"},
			{"old_string": "baz", "new_string": "BAZ"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "FOO\nbar\nBAZ\n" {
		t.Errorf("unexpected content after multi-edit: %q", data)
	}
}

func TestEditFile_RejectsAbsentString(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("hello world\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"file_path": path,
		"edits": []map[string]string{
			{"old_string": "goodbye world", "new_string": "hi"},
		},
	})
	if err == nil {
		t.Fatal("expected error for absent old_str")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got: %v", err)
	}
}

// When the file mtime has advanced past the agent's recorded read, the error
// should say so explicitly and print both mtimes — so the agent re-reads
// instead of retrying with cosmetic snippet variations.
func TestEditFile_NotFound_NamesMtimeDriftWhenFileChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.go")
	_ = os.WriteFile(path, []byte("hello\n"), 0o644)

	tracker := NewReadTracker()
	if _, err := NewReadFile(tracker).Execute(context.Background(), mustJSON(map[string]any{"file_path": path})); err != nil {
		t.Fatalf("read failed: %v", err)
	}

	// Modify the file out-of-band and bump mtime forward.
	if err := os.WriteFile(path, []byte("different content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	_, err := NewEditFile(WriteDeps{Reads: tracker}).Execute(context.Background(), mustJSON(map[string]any{
		"file_path": path,
		"edits":     []map[string]string{{"old_string": "hello", "new_string": "world"}},
	}))
	if err == nil {
		t.Fatal("expected not-found error")
	}
	msg := err.Error()
	for _, want := range []string{"modified since you read", "your read mtime:", "current mtime:"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q\nfull error: %s", want, msg)
		}
	}
}

// When the file has not changed since the agent's read but the snippet still
// doesn't match, the error should attribute the failure to the snippet (so
// the agent doesn't waste a tool call re-reading content it already has).
func TestEditFile_NotFound_BlamesSnippetWhenFileUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.go")
	_ = os.WriteFile(path, []byte("hello\n"), 0o644)

	tracker := NewReadTracker()
	if _, err := NewReadFile(tracker).Execute(context.Background(), mustJSON(map[string]any{"file_path": path})); err != nil {
		t.Fatalf("read failed: %v", err)
	}

	_, err := NewEditFile(WriteDeps{Reads: tracker}).Execute(context.Background(), mustJSON(map[string]any{
		"file_path": path,
		"edits":     []map[string]string{{"old_string": "goodbye", "new_string": "world"}},
	}))
	if err == nil {
		t.Fatal("expected not-found error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "unchanged since your read") {
		t.Errorf("expected 'unchanged since your read' in error, got: %s", msg)
	}
	if !strings.Contains(msg, "snippet is incorrect") {
		t.Errorf("expected snippet-is-incorrect framing in error, got: %s", msg)
	}
}

// When matchLineEndings transforms old_str (here: file is CRLF, agent sent
// LF), the error should include both the as-sent and the as-searched forms
// so the agent can see the normalisation that happened.
func TestEditFile_NotFound_ShowsBothSentAndSearchedSnippets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("alpha\r\nbeta\r\n"), 0o644)

	// Send an LF snippet that won't match for content reasons. matchLineEndings
	// will normalise LF → CRLF in old_str; the searched form differs from
	// the sent form, so the error should surface both.
	_, err := callEditFile(t, map[string]any{
		"file_path": path,
		"edits":     []map[string]string{{"old_string": "gamma\nbeta", "new_string": "delta"}},
	})
	if err == nil {
		t.Fatal("expected not-found error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "old_string:") {
		t.Errorf("expected as-sent old_str in error, got: %s", msg)
	}
	if !strings.Contains(msg, "searched (after newline normalisation):") {
		t.Errorf("expected searched form in error, got: %s", msg)
	}
	// %q renders \r\n explicitly; that's the proof the normalisation ran.
	if !strings.Contains(msg, `\r\n`) {
		t.Errorf("expected \\r\\n in searched form, got: %s", msg)
	}
}

func TestEditFile_RejectsAmbiguousString(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("foo\nfoo\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"file_path": path,
		"edits": []map[string]string{
			{"old_string": "foo", "new_string": "bar"},
		},
	})
	if err == nil {
		t.Fatal("expected error for ambiguous old_str")
	}
	if !strings.Contains(err.Error(), "appears 2 times") {
		t.Errorf("expected count in error, got: %v", err)
	}
}

func TestEditFile_RejectsEmptyOldStr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("content\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"file_path": path,
		"edits": []map[string]string{
			{"old_string": "", "new_string": "something"},
		},
	})
	if err == nil {
		t.Fatal("expected error for empty old_str")
	}
}

func TestEditFile_RejectsMissingFile(t *testing.T) {
	_, err := callEditFile(t, map[string]any{
		"file_path": "/nonexistent/file.txt",
		"edits": []map[string]string{
			{"old_string": "x", "new_string": "y"},
		},
	})
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestEditFile_AtomicOnSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("original content\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"file_path": path,
		"edits": []map[string]string{
			{"old_string": "original content", "new_string": "new content"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No .plumb.tmp sibling should remain next to the target.
	if _, err := os.Stat(path + ".plumb.tmp"); !os.IsNotExist(err) {
		t.Error("sibling tmp file should not exist after successful write")
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "new content") {
		t.Errorf("unexpected content: %q", data)
	}
}

func TestEditFile_FileUnchangedOnEditFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	original := "line1\nline2\nline3\n"
	_ = os.WriteFile(path, []byte(original), 0o644)

	// Second edit references something that doesn't exist after first edit.
	_, err := callEditFile(t, map[string]any{
		"file_path": path,
		"edits": []map[string]string{
			{"old_string": "line1", "new_string": "LINE1"},
			{"old_string": "NONEXISTENT", "new_string": "oops"},
		},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	// File must be unchanged — no partial write.
	data, _ := os.ReadFile(path)
	if string(data) != original {
		t.Errorf("file should be unchanged after failed multi-edit, got: %q", data)
	}
}

func TestEditFile_ExpectedSHA_AcceptsCorrect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("hello\n"), 0o644)

	sha, err := fileSHA256(path)
	if err != nil {
		t.Fatalf("fileSHA256: %v", err)
	}

	_, err = callEditFile(t, map[string]any{
		"file_path":    path,
		"expected_sha": sha,
		"edits":        []map[string]string{{"old_string": "hello", "new_string": "world"}},
	})
	if err != nil {
		t.Fatalf("unexpected error with correct expected_sha: %v", err)
	}
}

func TestEditFile_ExpectedSHA_RejectsStale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("hello\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"file_path":    path,
		"expected_sha": "0000000000000000000000000000000000000000000000000000000000000000",
		"edits":        []map[string]string{{"old_string": "hello", "new_string": "world"}},
	})
	if err == nil {
		t.Fatal("expected error for wrong expected_sha")
	}
	if !strings.Contains(err.Error(), "content has changed") {
		t.Errorf("expected 'content has changed' in error, got: %v", err)
	}
}

func TestEditFile_ApplyPartial_AllSucceed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.go")
	_ = os.WriteFile(path, []byte("aaa\nbbb\nccc\n"), 0o644)

	out, err := callEditFile(t, map[string]any{
		"file_path":     path,
		"apply_partial": true,
		"edits": []map[string]string{
			{"old_string": "aaa", "new_string": "AAA"},
			{"old_string": "bbb", "new_string": "BBB"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "applied 2 of 2") {
		t.Errorf("expected '2 of 2' in output; got: %s", out)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "AAA\nBBB\nccc\n" {
		t.Errorf("unexpected file content: %q", got)
	}
}

func TestEditFile_ApplyPartial_SomeFail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.go")
	_ = os.WriteFile(path, []byte("aaa\nbbb\nccc\n"), 0o644)

	out, err := callEditFile(t, map[string]any{
		"file_path":     path,
		"apply_partial": true,
		"edits": []map[string]string{
			{"old_string": "aaa", "new_string": "AAA"},   // succeeds
			{"old_string": "MISSING", "new_string": "X"}, // fails
			{"old_string": "ccc", "new_string": "CCC"},   // succeeds
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "applied 2 of 3") {
		t.Errorf("expected '2 of 3' in output; got: %s", out)
	}
	if !strings.Contains(out, "[1] FAILED") {
		t.Errorf("expected [1] FAILED in output; got: %s", out)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "AAA\nbbb\nCCC\n" {
		t.Errorf("unexpected file content: %q", got)
	}
}

func TestEditFile_DirtyCheck_RefusesDirtyFile(t *testing.T) {
	dir := initGitRepo(t)
	path := filepath.Join(dir, "f.go")
	_ = os.WriteFile(path, []byte("package main\n"), 0o644)
	gitExec(t, dir, "add", "f.go")
	gitExec(t, dir, "commit", "-m", "add")
	// Modify after commit to make dirty.
	_ = os.WriteFile(path, []byte("package main\n// modified\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"file_path": path,
		"edits":     []map[string]string{{"old_string": "package main", "new_string": "package foo"}},
	})
	if err == nil {
		t.Fatal("expected dirty file rejection")
	}
	if !strings.Contains(err.Error(), "uncommitted changes") {
		t.Errorf("unexpected error: %v", err)
	}
	// File must be unchanged.
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "modified") {
		t.Error("file content changed unexpectedly")
	}
}

func TestEditFile_ShowWriteDiff_IncludesDiff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.go")
	_ = os.WriteFile(path, []byte("package main\n\nfunc Old() {}\n"), 0o644)

	out, err := NewEditFile(WriteDeps{
		Reads:         NewReadTracker(),
		ShowWriteDiff: true,
	}).Execute(context.Background(), mustJSON(map[string]any{
		"file_path": path,
		"edits":     []map[string]string{{"old_string": "Old", "new_string": "New"}},
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "-func Old()") {
		t.Errorf("expected deletion line in diff; got:\n%s", out)
	}
	if !strings.Contains(out, "+func New()") {
		t.Errorf("expected addition line in diff; got:\n%s", out)
	}
}

func TestEditFile_ApplyPartial_AllFail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.go")
	content := "aaa\nbbb\n"
	_ = os.WriteFile(path, []byte(content), 0o644)

	out, err := callEditFile(t, map[string]any{
		"file_path":     path,
		"apply_partial": true,
		"edits":         []map[string]string{{"old_string": "MISSING", "new_string": "X"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "all edits failed") {
		t.Errorf("expected 'all edits failed' in output; got: %s", out)
	}
	got, _ := os.ReadFile(path)
	if string(got) != content {
		t.Error("file should be unchanged when all edits fail")
	}
}

func TestEditFile_RangeEdit_DeleteMiddle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("line1\nline2\nline3\nline4\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"file_path": path,
		"edits":     []map[string]any{{"start_line": 2, "end_line": 3, "new_string": ""}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "line1\nline4\n" {
		t.Errorf("want %q, got %q", "line1\nline4\n", string(got))
	}
}

func TestEditFile_RangeEdit_ReplaceLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("line1\nline2\nline3\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"file_path": path,
		"edits":     []map[string]any{{"start_line": 2, "end_line": 2, "new_string": "replaced\n"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "line1\nreplaced\nline3\n" {
		t.Errorf("want %q, got %q", "line1\nreplaced\nline3\n", string(got))
	}
}

func TestEditFile_RangeEdit_AppendEOF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("line1\nline2\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"file_path": path,
		"edits":     []map[string]any{{"start_line": -1, "new_string": "line3\n"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "line1\nline2\nline3\n" {
		t.Errorf("want %q, got %q", "line1\nline2\nline3\n", string(got))
	}
}

func TestEditFile_RangeEdit_DeleteToEOF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("line1\nline2\nline3\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"file_path": path,
		"edits":     []map[string]any{{"start_line": 2, "end_line": -1, "new_string": ""}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "line1\n" {
		t.Errorf("want %q, got %q", "line1\n", string(got))
	}
}

func TestEditFile_RangeEdit_OutOfRange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("line1\nline2\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"file_path": path,
		"edits":     []map[string]any{{"start_line": 99, "new_string": "x\n"}},
	})
	if err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("expected out-of-range error, got: %v", err)
	}
}

func TestEditFile_StaleReadWarning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reads := NewReadTracker()
	info, _ := os.Stat(path)
	reads.Record(path, info.ModTime()) // this session read the original
	tool := NewEditFile(WriteDeps{Reads: reads})

	// A peer appends to the file after our read; the edited region ("beta") stays.
	future := time.Now().Add(2 * time.Second)
	_ = os.WriteFile(path, []byte("alpha\nbeta\ngamma\nDELTA\n"), 0o644)
	_ = os.Chtimes(path, future, future)

	raw, _ := json.Marshal(map[string]any{
		"file_path": path,
		"edits":     []map[string]any{{"old_string": "beta", "new_string": "BETA"}},
	})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("edit should apply (anchor still matches): %v", err)
	}
	if !strings.Contains(out, "plumb-warn") || !strings.Contains(out, "changed on disk since your session last read it") {
		t.Errorf("expected a stale-read warning, got: %q", out)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), "BETA") {
		t.Errorf("edit should have applied, got: %q", data)
	}
}

func TestEditFile_NoStaleWarningWhenUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("alpha\nbeta\n"), 0o644)
	reads := NewReadTracker()
	info, _ := os.Stat(path)
	reads.Record(path, info.ModTime())
	tool := NewEditFile(WriteDeps{Reads: reads})
	raw, _ := json.Marshal(map[string]any{
		"file_path": path,
		"edits":     []map[string]any{{"old_string": "beta", "new_string": "BETA"}},
	})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if strings.Contains(out, "plumb-warn") {
		t.Errorf("an unchanged file should carry no stale-read warning, got: %q", out)
	}
}
