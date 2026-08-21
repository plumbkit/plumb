package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestEditFileRejection is the PLAN-358 acceptance test: every edit_file
// rejection carries enough information for a one-call retry.
func TestEditFileRejection(t *testing.T) {
	t.Run("multi-line miss suggests RANGE mode with computed line numbers", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f.go")
		before := "func f() {\n\treturn 42\n}\n\nfunc g() {\n\treturn 43\n}\n"
		if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
			t.Fatal(err)
		}

		// Same block, only the indentation drifted — a near-exact 3-line window
		// exists at lines 1-3, so the fuzzy locate should find it.
		wrongIndent := "func f() {\n    return 42\n}\n"
		_, err := callEditFile(t, map[string]any{
			"file_path": path,
			"edits":     []map[string]any{{"old_string": wrongIndent, "new_string": "x"}},
		})
		if err == nil {
			t.Fatal("expected old_string not found error")
		}
		msg := err.Error()
		for _, want := range []string{
			"Closest match: lines 1", "similarity ~", "RANGE mode",
			`"start_line": 1`, `"end_line": 3`,
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("rejection missing %q\nfull error: %s", want, msg)
			}
		}

		// Mutate-and-confirm: a suggestion must never modify the file.
		got, _ := os.ReadFile(path)
		if string(got) != before {
			t.Fatalf("rejected edit modified the file: %q", got)
		}
	})

	t.Run("multi-line miss with no near match still points at RANGE mode", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f.go")
		before := "package main\n\nfunc main() {}\n"
		if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := callEditFile(t, map[string]any{
			"file_path": path,
			"edits": []map[string]any{{
				"old_string": "the quick brown fox\njumps over\nthe lazy dog\n",
				"new_string": "x",
			}},
		})
		if err == nil {
			t.Fatal("expected old_string not found error")
		}
		msg := err.Error()
		if !strings.Contains(msg, "RANGE mode") {
			t.Errorf("rejection missing the generic RANGE-mode pointer\nfull error: %s", msg)
		}
		if strings.Contains(msg, "Closest match:") {
			t.Errorf("no near match exists — must not claim computed line numbers\nfull error: %s", msg)
		}

		got, _ := os.ReadFile(path)
		if string(got) != before {
			t.Fatalf("rejected edit modified the file: %q", got)
		}
	})

	t.Run("short old_string miss does not get the multi-line hint", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f.go")
		before := "hello\nworld\n"
		if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := callEditFile(t, map[string]any{
			"file_path": path,
			"edits":     []map[string]any{{"old_string": "hallo", "new_string": "x"}},
		})
		if err == nil {
			t.Fatal("expected old_string not found error")
		}
		if msg := err.Error(); strings.Contains(msg, "RANGE mode") {
			t.Errorf("a 1-line old_string should not trigger the RANGE-mode hint\nfull error: %s", msg)
		}
	})

	t.Run("modified-since-read rejection carries mtime, sha, and reconcile", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f.go")
		if err := os.WriteFile(path, []byte("hello\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		staleMtime := time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
		before, _ := os.ReadFile(path)

		_, err := callEditFile(t, map[string]any{
			"file_path":      path,
			"expected_mtime": staleMtime,
			"edits":          []map[string]any{{"old_string": "hello", "new_string": "world"}},
		})
		if err == nil {
			t.Fatal("expected stale expected_mtime rejection")
		}
		msg := err.Error()
		for _, want := range []string{"current mtime:", "current sha256:", "reconcile"} {
			if !strings.Contains(msg, want) {
				t.Errorf("rejection missing %q\nfull error: %s", want, msg)
			}
		}

		// Mutate-and-confirm: the rejected edit must leave the file untouched.
		after, _ := os.ReadFile(path)
		if string(after) != string(before) {
			t.Fatalf("rejected edit modified the file: %q", after)
		}
	})

	t.Run("modified-since-read via expected_sha also carries mtime and sha", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f.go")
		if err := os.WriteFile(path, []byte("hello\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(path)

		_, err := callEditFile(t, map[string]any{
			"file_path":    path,
			"expected_sha": "0000000000000000000000000000000000000000000000000000000000000000",
			"edits":        []map[string]any{{"old_string": "hello", "new_string": "world"}},
		})
		if err == nil {
			t.Fatal("expected stale expected_sha rejection")
		}
		msg := err.Error()
		for _, want := range []string{"current  sha256:", "current mtime:", "reconcile"} {
			if !strings.Contains(msg, want) {
				t.Errorf("rejection missing %q\nfull error: %s", want, msg)
			}
		}

		after, _ := os.ReadFile(path)
		if string(after) != string(before) {
			t.Fatalf("rejected edit modified the file: %q", after)
		}
	})
}
