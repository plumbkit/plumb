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

// largeBody builds content over the large-read nudge threshold.
func largeBody() []byte {
	return []byte(strings.Repeat("# section\nsome content line here\n", 2000)) // ~58 KiB
}

func TestReadFile_LargeReadNudge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.md")
	_ = os.WriteFile(path, largeBody(), 0o644)
	tool := NewReadFile(nil).WithOutlineHint(func(string) bool { return true })

	raw, _ := json.Marshal(map[string]any{"file_path": path})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if !strings.Contains(out, "file_outline") || !strings.Contains(out, "large file") {
		t.Fatalf("expected a large-read file_outline nudge:\n%s", out[:200])
	}
}

func TestReadFile_LargeReadNudge_Suppressed(t *testing.T) {
	large := largeBody()
	cases := []struct {
		name string
		tool *ReadFile
		args map[string]any
		body []byte
	}{
		{"no structural engine", NewReadFile(nil).WithOutlineHint(func(string) bool { return false }), map[string]any{}, large},
		{"ranged read", NewReadFile(nil).WithOutlineHint(func(string) bool { return true }), map[string]any{"start_line": 1, "end_line": 50}, large},
		{"small file", NewReadFile(nil).WithOutlineHint(func(string) bool { return true }), map[string]any{}, []byte("tiny\n")},
		{"no hint wired", NewReadFile(nil), map[string]any{}, large},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "f.md")
			_ = os.WriteFile(path, c.body, 0o644)
			c.args["file_path"] = path
			raw, _ := json.Marshal(c.args)
			out, err := c.tool.Execute(context.Background(), raw)
			if err != nil {
				t.Fatalf("read_file: %v", err)
			}
			if strings.Contains(out, "large file") {
				t.Fatalf("nudge must be suppressed for %s:\n%s", c.name, out[:200])
			}
		})
	}
}

func TestReadFile_HeaderLineAndCharCounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "multibyte.txt")
	// 3 lines; contains multibyte glyphs so chars < bytes.
	_ = os.WriteFile(path, []byte("a → b\nc — d\ne\n"), 0o644)

	out, err := callReadFile(t, map[string]any{"file_path": path})
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	head := out[:strings.IndexByte(out, '\n')]
	if !strings.Contains(head, "lines=3") {
		t.Errorf("expected lines=3 in header, got: %q", head)
	}
	if !strings.Contains(head, "chars=") {
		t.Errorf("expected a chars= field in header, got: %q", head)
	}
	// The byte length (18) exceeds the rune count (14) for this multibyte body.
	if !strings.Contains(head, "chars=14") {
		t.Errorf("expected chars=14 (rune count, not bytes) in header, got: %q", head)
	}
}

func TestReadFile_ConcurrentEditWarning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.go")
	if err := os.WriteFile(path, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writes := NewWriteTracker()
	writes.Record(path) // plumb "wrote" it: captures the current mtime

	tool := NewReadFile(nil).WithWrites(writes)

	// A read with no external change carries no warning.
	out, err := callReadFileWith(t, tool, path)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if strings.Contains(out, "plumb-warn") {
		t.Errorf("unchanged file should carry no concurrent-edit warning:\n%s", out)
	}

	// Simulate a peer editing the file after plumb's write (advance mtime).
	future := time.Now().Add(2 * time.Second)
	if err := os.WriteFile(path, []byte("v2 peer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	out, err = callReadFileWith(t, tool, path)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if !strings.Contains(out, "plumb-warn") || !strings.Contains(out, "changed on disk") {
		t.Errorf("expected a concurrent-edit warning after an external write:\n%s", out)
	}
}

// callReadFileWith executes a pre-built ReadFile tool against path.
func callReadFileWith(t *testing.T, tool *ReadFile, path string) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"file_path": path})
	return tool.Execute(context.Background(), raw)
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

func TestReadFile_LineNumberGutter(t *testing.T) {
	// Whole-file read: gutter starts at 1 and the body content is preserved
	// verbatim after the "<n>\t" prefix.
	path := writeTextFile(t, "alpha\nbeta\ngamma\n")
	out, err := callReadFile(t, map[string]any{"file_path": path})
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	_, body, _ := strings.Cut(out, "\n\n") // header block, blank line, then content
	want := "1\talpha\n2\tbeta\n3\tgamma\n"
	if body != want {
		t.Fatalf("gutter mismatch:\n got %q\nwant %q", body, want)
	}
}

func TestReadFile_GutterRangeStartsAtFileLine(t *testing.T) {
	// A sliced read numbers lines by their real file position, not 1.
	path := writeTextFile(t, "l1\nl2\nl3\nl4\nl5\n")
	start, end := 3, 4
	out, err := callReadFile(t, map[string]any{"file_path": path, "start_line": start, "end_line": end})
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if !strings.Contains(out, "3\tl3") || !strings.Contains(out, "4\tl4") {
		t.Fatalf("expected gutters 3/4 keyed to file lines, got:\n%s", out)
	}
}

func TestWithLineGutter(t *testing.T) {
	cases := []struct {
		name      string
		content   string
		firstLine int
		want      string
	}{
		{"empty stays empty", "", 1, ""},
		{"trailing newline kept, no phantom line", "a\nb\n", 1, "1\ta\n2\tb\n"},
		{"no trailing newline", "a\nb", 1, "1\ta\n2\tb"},
		{"width grows with line number", "x\ny", 9, " 9\tx\n10\ty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := withLineGutter(c.content, c.firstLine); got != c.want {
				t.Fatalf("withLineGutter(%q, %d) = %q, want %q", c.content, c.firstLine, got, c.want)
			}
		})
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

// TestReadFile_EditLaneHint_ClaudeCode verifies that a Claude Code client gets
// the edit_file call-to-action as a second header comment line, carrying the
// exact mtime so the follow-up edit is copy-paste ready. The plumb-read header
// must remain the first line (other tooling parses it).
func TestReadFile_EditLaneHint_ClaudeCode(t *testing.T) {
	path := writeTextFile(t, "hello\nworld\n")
	raw, _ := json.Marshal(map[string]any{"file_path": path})
	tool := NewReadFile(nil).WithClient(func() string { return "claude-code" })
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(out, "# plumb-read mtime=") {
		t.Fatalf("plumb-read header must remain the first line, got: %q", out)
	}
	lines := strings.SplitN(out, "\n", 3)
	if len(lines) < 2 || !strings.HasPrefix(lines[1], "# ") {
		t.Fatalf("expected the edit-lane hint as the second comment line, got: %q", out)
	}
	hintLine := lines[1]
	for _, want := range []string{"edit_file", "native Edit", "expected_mtime"} {
		if !strings.Contains(hintLine, want) {
			t.Errorf("hint line missing %q: %q", want, hintLine)
		}
	}
	// The mtime in the hint must equal the mtime in the header (copy-paste ready).
	headerMtime := extractMtime(t, lines[0])
	if headerMtime != "" && !strings.Contains(hintLine, headerMtime) {
		t.Errorf("hint mtime should match header mtime %q: %q", headerMtime, hintLine)
	}
	// Content must still be present after the header block.
	if !strings.Contains(out, "hello") || !strings.Contains(out, "world") {
		t.Errorf("file content missing from output:\n%s", out)
	}
}

// TestReadFile_NoEditLaneHint_OtherClients verifies the hint is suppressed for
// clients without the native-edit conflict (and when no client is wired), so
// their read output stays lean: header line, blank line, then content.
func TestReadFile_NoEditLaneHint_OtherClients(t *testing.T) {
	cases := []struct {
		name   string
		client func() string
	}{
		{"nil client", nil},
		{"claude desktop", func() string { return "claude-ai" }},
		{"vscode", func() string { return "vscode" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeTextFile(t, "hello\n")
			raw, _ := json.Marshal(map[string]any{"file_path": path})
			tool := NewReadFile(nil)
			if c.client != nil {
				tool = tool.WithClient(c.client)
			}
			out, err := tool.Execute(context.Background(), raw)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if strings.Contains(out, "edit_file") || strings.Contains(out, "native Edit") {
				t.Errorf("non-conflict client must not get the edit-lane hint:\n%s", out)
			}
			// Lean format: header line, then blank line, then content.
			lines := strings.SplitN(out, "\n", 3)
			if len(lines) < 2 || lines[1] != "" {
				t.Errorf("expected a blank line after the header, got: %q", out)
			}
		})
	}
}

// extractMtime pulls the mtime= value out of a plumb-read header line.
func extractMtime(t *testing.T, header string) string {
	t.Helper()
	const key = "mtime="
	i := strings.Index(header, key)
	if i < 0 {
		return ""
	}
	rest := header[i+len(key):]
	if j := strings.IndexByte(rest, ' '); j >= 0 {
		return rest[:j]
	}
	return rest
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

func TestReadFile_TruncationSuggestsOutline(t *testing.T) {
	// A file larger than the 200 KiB cap is truncated; the truncation note should
	// point at file_outline as the one-call alternative to a blind whole-file read.
	big := strings.Repeat("package x // filler line to exceed the read cap\n", 6000) // ~270 KiB
	path := writeTextFile(t, big)
	out, err := callReadFile(t, map[string]any{"file_path": path})
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if !strings.Contains(out, "output truncated") {
		t.Fatalf("expected a truncation note for an over-cap file")
	}
	if !strings.Contains(out, "file_outline") {
		t.Errorf("truncation note should suggest file_outline, got tail: %q", out[len(out)-200:])
	}
}

// --- pattern (in-file search) mode ---------------------------------------

func TestReadFile_Search_MatchesWithLineNumbers(t *testing.T) {
	content := "alpha\nbeta target here\ngamma\ndelta target again\nepsilon\n"
	path := writeTextFile(t, content)
	out, err := callReadFile(t, map[string]any{"file_path": path, "pattern": "target"})
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if !strings.Contains(out, "# plumb-search: 2 matches for \"target\"") {
		t.Fatalf("expected a 2-match summary, got:\n%s", out)
	}
	// Matches carry their real 1-based file line numbers via the gutter.
	if !strings.Contains(out, "2\tbeta target here") {
		t.Errorf("expected line 2 with its gutter, got:\n%s", out)
	}
	if !strings.Contains(out, "4\tdelta target again") {
		t.Errorf("expected line 4 with its gutter, got:\n%s", out)
	}
	// Non-matching lines are absent (no context requested).
	if strings.Contains(out, "gamma") {
		t.Errorf("non-matching line should not appear, got:\n%s", out)
	}
}

func TestReadFile_Search_OverCapFileSearchable(t *testing.T) {
	// Build a file far larger than the 200 KiB read cap, with the needle near the
	// very end — beyond where a whole-file read would be truncated.
	var sb strings.Builder
	for i := 1; i <= 12000; i++ {
		sb.WriteString("filler padding line to grow the file well past the cap\n")
	}
	sb.WriteString("UNIQUENEEDLE lives down here\n")
	path := writeTextFile(t, sb.String())

	// Sanity: a plain read of this file is truncated at the cap.
	plain, err := callReadFile(t, map[string]any{"file_path": path})
	if err != nil {
		t.Fatalf("read_file (plain): %v", err)
	}
	if !strings.Contains(plain, "output truncated") {
		t.Fatalf("expected the plain read to be over-cap/truncated")
	}
	if strings.Contains(plain, "UNIQUENEEDLE") {
		t.Fatalf("needle should be beyond the read cap in a plain read")
	}

	// Search finds it regardless of size.
	out, err := callReadFile(t, map[string]any{"file_path": path, "pattern": "UNIQUENEEDLE"})
	if err != nil {
		t.Fatalf("read_file (search): %v", err)
	}
	if !strings.Contains(out, "UNIQUENEEDLE lives down here") {
		t.Fatalf("search should locate the needle in an over-cap file, got tail:\n%s", out[len(out)-200:])
	}
	if !strings.Contains(out, "12001\tUNIQUENEEDLE lives down here") {
		t.Errorf("expected the needle at line 12001 with its gutter, got tail:\n%s", out[len(out)-200:])
	}
}

func TestReadFile_Search_ContextLines(t *testing.T) {
	content := "line1\nline2\nMATCH_A\nline4\nline5\nline6\nMATCH_B\nline8\n"
	path := writeTextFile(t, content)
	out, err := callReadFile(t, map[string]any{"file_path": path, "pattern": "MATCH_", "context_lines": 1})
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	// One line of context each side of both matches.
	for _, want := range []string{"2\tline2", "3\tMATCH_A", "4\tline4", "6\tline6", "7\tMATCH_B", "8\tline8"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected context/match line %q, got:\n%s", want, out)
		}
	}
	// The two disjoint groups are separated by an rg-style "--" line.
	if !strings.Contains(out, "\n--\n") {
		t.Errorf("expected a -- separator between disjoint groups, got:\n%s", out)
	}
	// A line outside either context window is not shown.
	if strings.Contains(out, "line5") {
		t.Errorf("line5 is outside both context windows and should be absent, got:\n%s", out)
	}
}

func TestReadFile_Search_MaxMatchesTruncationLabelled(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 10; i++ {
		sb.WriteString("hit line\n")
	}
	path := writeTextFile(t, sb.String())
	out, err := callReadFile(t, map[string]any{"file_path": path, "pattern": "hit", "max_matches": 3})
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if !strings.Contains(out, "search output truncated at 3 matches") {
		t.Fatalf("expected a truncation label at the match cap, got:\n%s", out)
	}
	if !strings.Contains(out, "first 3 matches") {
		t.Errorf("summary should note only the first 3 matches are shown, got:\n%s", out)
	}
	// Exactly the first three matches (lines 1–3) are rendered.
	if strings.Contains(out, "4\thit line") {
		t.Errorf("a fourth match should not be rendered past the cap, got:\n%s", out)
	}
}

func TestReadFile_Search_WithinLineRange(t *testing.T) {
	content := "needle up top\nfiller\nfiller\nneedle in the middle\nfiller\nneedle at bottom\n"
	path := writeTextFile(t, content)
	out, err := callReadFile(t, map[string]any{
		"file_path":  path,
		"pattern":    "needle",
		"start_line": 2,
		"end_line":   5,
	})
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if !strings.Contains(out, "within lines 2–5") {
		t.Errorf("summary should name the restricted window, got:\n%s", out)
	}
	if !strings.Contains(out, "4\tneedle in the middle") {
		t.Errorf("the in-window match should be found, got:\n%s", out)
	}
	// Matches outside the [2,5] window are excluded.
	if strings.Contains(out, "needle up top") || strings.Contains(out, "needle at bottom") {
		t.Errorf("out-of-window matches should be excluded, got:\n%s", out)
	}
	if !strings.Contains(out, "# plumb-search: 1 match for") {
		t.Errorf("expected exactly one match within the window, got:\n%s", out)
	}
}

func TestReadFile_Search_LimitRejected(t *testing.T) {
	path := writeTextFile(t, "one match here\n")
	_, err := callReadFile(t, map[string]any{"file_path": path, "pattern": "match", "limit": 5})
	if err == nil {
		t.Fatalf("expected an error combining pattern with limit")
	}
	if !strings.Contains(err.Error(), "max_matches") {
		t.Errorf("error should point to max_matches, got: %v", err)
	}
}

func TestReadFile_Search_NoMatch(t *testing.T) {
	path := writeTextFile(t, "alpha\nbeta\ngamma\n")
	out, err := callReadFile(t, map[string]any{"file_path": path, "pattern": "zeta"})
	if err != nil {
		t.Fatalf("a no-match search is not an error, got: %v", err)
	}
	if !strings.Contains(out, "No matches for \"zeta\" in") {
		t.Fatalf("expected an explicit no-match message, got:\n%s", out)
	}
	if !strings.Contains(out, "# plumb-search: no matches") {
		t.Errorf("expected a no-match summary line, got:\n%s", out)
	}
}

func TestReadFile_Search_InvalidRegex(t *testing.T) {
	path := writeTextFile(t, "anything\n")
	_, err := callReadFile(t, map[string]any{"file_path": path, "pattern": "foo(", "use_regex": true})
	if err == nil {
		t.Fatalf("expected a clean error for an invalid regex")
	}
	if !strings.Contains(err.Error(), "invalid regex") {
		t.Errorf("error should name the invalid regex, got: %v", err)
	}
}

func TestReadFile_Search_SmartCaseAndCaseSensitive(t *testing.T) {
	content := "has Needle here\nhas needle there\n"
	path := writeTextFile(t, content)
	tests := []struct {
		name          string
		pattern       string
		caseSensitive *bool
		wantSummary   string
	}{
		{"smartcase lowercase matches both", "needle", nil, "2 matches"},
		{"mixed-case is case-sensitive", "Needle", nil, "1 match"},
		{"forced sensitive lowercase", "needle", boolPtr(true), "1 match"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"file_path": path, "pattern": tc.pattern}
			if tc.caseSensitive != nil {
				args["case_sensitive"] = *tc.caseSensitive
			}
			out, err := callReadFile(t, args)
			if err != nil {
				t.Fatalf("read_file: %v", err)
			}
			if !strings.Contains(out, tc.wantSummary) {
				t.Errorf("want summary %q, got:\n%s", tc.wantSummary, out)
			}
		})
	}
}

func TestReadFile_Search_RegexLiteralVsRegex(t *testing.T) {
	// 'a.c' should match only the literal "a.c" in literal mode, but "abc" too as a regex.
	content := "a.c literal\nabc regex-only\n"
	path := writeTextFile(t, content)

	lit, err := callReadFile(t, map[string]any{"file_path": path, "pattern": "a.c"})
	if err != nil {
		t.Fatalf("read_file (literal): %v", err)
	}
	if !strings.Contains(lit, "# plumb-search: 1 match for") || strings.Contains(lit, "abc regex-only") {
		t.Errorf("literal mode should match only the literal a.c, got:\n%s", lit)
	}

	re, err := callReadFile(t, map[string]any{"file_path": path, "pattern": "a.c", "use_regex": true})
	if err != nil {
		t.Fatalf("read_file (regex): %v", err)
	}
	if !strings.Contains(re, "# plumb-search: 2 matches for") {
		t.Errorf("regex mode should match both lines, got:\n%s", re)
	}
}

func boolPtr(b bool) *bool { return &b }
