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
		"path":  path,
		"edits": []map[string]string{{"old_str": "hello", "new_str": "world"}},
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
	_, _ = NewReadFile(tracker).Execute(context.Background(), mustJSON(map[string]any{"path": path}))
	out, err := NewEditFile(WriteDeps{Reads: tracker, Strict: func() bool { return true }}).
		Execute(context.Background(), mustJSON(map[string]any{
			"path":  path,
			"edits": []map[string]string{{"old_str": "hello", "new_str": "world"}},
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
	_, _ = NewReadFile(sessionA).Execute(context.Background(), mustJSON(map[string]any{"path": path}))

	_, err := NewEditFile(WriteDeps{Reads: sessionB, Strict: func() bool { return true }}).
		Execute(context.Background(), mustJSON(map[string]any{
			"path":  path,
			"edits": []map[string]string{{"old_str": "hello", "new_str": "world"}},
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

func TestEditFile_BasicReplace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.go")
	_ = os.WriteFile(path, []byte("package main\n\nfunc Hello() {}\n"), 0o644)

	out, err := callEditFile(t, map[string]any{
		"path": path,
		"edits": []map[string]string{
			{"old_str": "func Hello() {}", "new_str": "func Hello() { return }"},
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
		"path": path,
		"edits": []map[string]string{
			{"old_str": "foo", "new_str": "FOO"},
			{"old_str": "baz", "new_str": "BAZ"},
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
		"path": path,
		"edits": []map[string]string{
			{"old_str": "goodbye world", "new_str": "hi"},
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
	if _, err := NewReadFile(tracker).Execute(context.Background(), mustJSON(map[string]any{"path": path})); err != nil {
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
		"path":  path,
		"edits": []map[string]string{{"old_str": "hello", "new_str": "world"}},
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
	if _, err := NewReadFile(tracker).Execute(context.Background(), mustJSON(map[string]any{"path": path})); err != nil {
		t.Fatalf("read failed: %v", err)
	}

	_, err := NewEditFile(WriteDeps{Reads: tracker}).Execute(context.Background(), mustJSON(map[string]any{
		"path":  path,
		"edits": []map[string]string{{"old_str": "goodbye", "new_str": "world"}},
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
		"path":  path,
		"edits": []map[string]string{{"old_str": "gamma\nbeta", "new_str": "delta"}},
	})
	if err == nil {
		t.Fatal("expected not-found error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "old_str:") {
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
		"path": path,
		"edits": []map[string]string{
			{"old_str": "foo", "new_str": "bar"},
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
		"path": path,
		"edits": []map[string]string{
			{"old_str": "", "new_str": "something"},
		},
	})
	if err == nil {
		t.Fatal("expected error for empty old_str")
	}
}

func TestEditFile_RejectsMissingFile(t *testing.T) {
	_, err := callEditFile(t, map[string]any{
		"path": "/nonexistent/file.txt",
		"edits": []map[string]string{
			{"old_str": "x", "new_str": "y"},
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
		"path": path,
		"edits": []map[string]string{
			{"old_str": "original content", "new_str": "new content"},
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
		"path": path,
		"edits": []map[string]string{
			{"old_str": "line1", "new_str": "LINE1"},
			{"old_str": "NONEXISTENT", "new_str": "oops"},
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
