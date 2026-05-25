package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func writeTextFile(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "plumb-read-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	return f.Name()
}

func callReadFile(t *testing.T, args map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	return NewReadFile(nil).Execute(context.Background(), raw)
}

func TestReadFile_Basic(t *testing.T) {
	path := writeTextFile(t, "hello\nworld\n")
	out, err := callReadFile(t, map[string]any{"file_path": path})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "hello") || !strings.Contains(out, "world") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestReadFile_FileURI(t *testing.T) {
	path := writeTextFile(t, "content via URI\n")
	out, err := callReadFile(t, map[string]any{"file_path": "file://" + path})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "content via URI") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestReadFile_Limit(t *testing.T) {
	content := "line1\nline2\nline3\nline4\nline5\n"
	path := writeTextFile(t, content)

	tests := []struct {
		name    string
		args    map[string]any
		want    []string // lines expected present
		absent  []string // lines expected absent
		wantErr bool
	}{
		{"limit from line 1", map[string]any{"limit": 2}, []string{"line1", "line2"}, []string{"line3"}, false},
		{"start_line + limit window", map[string]any{"start_line": 3, "limit": 2}, []string{"line3", "line4"}, []string{"line2", "line5"}, false},
		{"offset is a start_line synonym", map[string]any{"offset": 4, "limit": 1}, []string{"line4"}, []string{"line3", "line5"}, false},
		{"limit and end_line conflict", map[string]any{"end_line": 4, "limit": 2}, nil, nil, true},
		{"limit must be >= 1", map[string]any{"limit": 0}, nil, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.args["file_path"] = path
			out, err := callReadFile(t, tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want error, got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			assertLines(t, out, tc.want, tc.absent)
		})
	}
}

func assertLines(t *testing.T, out string, want, absent []string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("want %q in:\n%s", w, out)
		}
	}
	for _, a := range absent {
		if strings.Contains(out, a) {
			t.Errorf("did not want %q in:\n%s", a, out)
		}
	}
}

func TestReadFile_LineRange(t *testing.T) {
	content := "line1\nline2\nline3\nline4\nline5\n"
	path := writeTextFile(t, content)

	start, end := 2, 4
	out, err := callReadFile(t, map[string]any{"file_path": path, "start_line": start, "end_line": end})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, "line1") || strings.Contains(out, "line5") {
		t.Fatalf("expected only lines 2–4, got: %q", out)
	}
	if !strings.Contains(out, "line2") || !strings.Contains(out, "line4") {
		t.Fatalf("expected lines 2–4 inclusive, got: %q", out)
	}
}

func TestReadFile_StartLineOnly(t *testing.T) {
	path := writeTextFile(t, "a\nb\nc\n")
	start := 2
	out, err := callReadFile(t, map[string]any{"file_path": path, "start_line": start})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, "a\n") {
		t.Fatalf("expected only lines from 2 onward, got: %q", out)
	}
	if !strings.Contains(out, "b") {
		t.Fatalf("expected line2+ in output, got: %q", out)
	}
}

func TestReadFile_OutOfRangeLines(t *testing.T) {
	path := writeTextFile(t, "one\ntwo\n")
	start, end := 10, 20
	out, err := callReadFile(t, map[string]any{"file_path": path, "start_line": start, "end_line": end})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "no lines") {
		t.Fatalf("expected 'no lines' message, got: %q", out)
	}
}

func TestReadFile_Directory(t *testing.T) {
	dir := t.TempDir()
	_, err := callReadFile(t, map[string]any{"file_path": dir})
	if err == nil {
		t.Fatal("expected error for directory path")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Fatalf("expected directory error message, got: %v", err)
	}
}

func TestReadFile_MissingFile(t *testing.T) {
	_, err := callReadFile(t, map[string]any{"file_path": "/nonexistent/path/file.txt"})
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestReadFile_MissingPath(t *testing.T) {
	_, err := callReadFile(t, map[string]any{})
	if err == nil {
		t.Fatal("expected error when path is empty")
	}
}

func TestReadFile_HeaderContainsSHA256(t *testing.T) {
	path := writeTextFile(t, "hello\nworld\n")
	out, err := callReadFile(t, map[string]any{"file_path": path})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	header, _, _ := strings.Cut(out, "\n")
	if !strings.Contains(header, "sha256=") {
		t.Fatalf("header missing sha256 field: %q", header)
	}
	// Extract and sanity-check the hash length (64 hex chars).
	hash := extractSHA(out)
	if len(hash) != 64 {
		t.Fatalf("sha256 field has wrong length %d: %q", len(hash), hash)
	}
}

func TestReadFile_SHA256ConsistentWithFullContent(t *testing.T) {
	// Range reads must return the SHA of the full file, not just the slice.
	path := writeTextFile(t, "line1\nline2\nline3\n")

	full, _ := callReadFile(t, map[string]any{"file_path": path})
	start, end := 2, 2
	partial, _ := callReadFile(t, map[string]any{"file_path": path, "start_line": start, "end_line": end})

	shaFull := extractSHA(full)
	shaPartial := extractSHA(partial)
	if shaFull == "" || shaPartial == "" {
		t.Fatal("could not extract sha256 from header")
	}
	if shaFull != shaPartial {
		t.Fatalf("sha256 must match for full and partial reads: full=%s partial=%s", shaFull, shaPartial)
	}
}

func extractSHA(out string) string {
	header, _, _ := strings.Cut(out, "\n")
	for field, rest, ok := strings.Cut(header, "sha256="); ok; field, rest, ok = strings.Cut(rest, "sha256=") {
		_ = field
		val, _, _ := strings.Cut(rest, " ")
		return val
	}
	return ""
}

func TestReadFile_OutputHasMtimeHeader(t *testing.T) {
	path := writeTextFile(t, "hello\n")
	out, err := callReadFile(t, map[string]any{"file_path": path})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(out, "# plumb-read mtime=") {
		head := out
		if len(head) > 80 {
			head = head[:80]
		}
		t.Fatalf("expected mtime header, got: %q", head)
	}
}

func TestReadFile_HeaderIncludesIndentStyle(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"tabs", "func f() {\n\tx := 1\n\treturn x\n}\n", "indent=tabs"},
		{"spaces", "def f():\n    x = 1\n    return x\n", "indent=spaces"},
		{"mixed", "block:\n\ttab line\n  space line\n", "indent=mixed"},
		{"none", "single line, no indent\n", "indent=none"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeTextFile(t, c.content)
			out, err := callReadFile(t, map[string]any{"file_path": path})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// Header is the first line; it should contain both the mtime
			// and the expected indent= field.
			head, _, _ := strings.Cut(out, "\n")
			if !strings.HasPrefix(head, "# plumb-read mtime=") {
				t.Fatalf("missing mtime in header: %q", head)
			}
			if !strings.Contains(head, c.want) {
				t.Fatalf("header missing %q: got %q", c.want, head)
			}
		})
	}
}

func TestReadFile_BinaryFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "plumb-bin-*")
	if err != nil {
		t.Fatal(err)
	}
	// Write null bytes to make it look binary.
	_, _ = f.Write(make([]byte, 100))
	_ = f.Close()

	_, err = callReadFile(t, map[string]any{"file_path": f.Name()})
	if err == nil {
		t.Fatal("expected error for binary file")
	}
	if !strings.Contains(err.Error(), "binary") {
		t.Fatalf("expected binary error message, got: %v", err)
	}
}
