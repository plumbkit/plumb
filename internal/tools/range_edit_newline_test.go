package tools

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestApplyRangeEdit_WholeLines pins #543: range mode replaces whole lines, so
// a non-empty new_string without a trailing newline gains the line ending of
// the text it replaced instead of being glued onto the following line. Every
// shape is run with and without the trailing newline; both spellings must
// produce the same file. The one exception is a range running to EOF in a
// file with no final newline, which must stay without one.
func TestApplyRangeEdit_WholeLines(t *testing.T) {
	const lf = "a\nb\nc\nd\n"
	const noFinal = "a\nb\nc"
	const crlf = "a\r\nb\r\nc\r\nd\r\n"
	cases := []struct {
		name       string
		content    string
		start, end int
		newStr     string
		want       string
	}{
		// 1 line -> 1 line.
		{"1to1 bare", lf, 2, 2, "B", "a\nB\nc\nd\n"},
		{"1to1 terminated", lf, 2, 2, "B\n", "a\nB\nc\nd\n"},
		// 1 line -> 2 lines (the reported merge-conflict shape).
		{"1to2 bare", lf, 2, 2, "B1\nB2", "a\nB1\nB2\nc\nd\n"},
		{"1to2 terminated", lf, 2, 2, "B1\nB2\n", "a\nB1\nB2\nc\nd\n"},
		// N lines -> 1 line, and N lines -> 0 lines.
		{"2to1 bare", lf, 2, 3, "BC", "a\nBC\nd\n"},
		{"2to1 terminated", lf, 2, 3, "BC\n", "a\nBC\nd\n"},
		{"2to0 delete", lf, 2, 3, "", "a\nd\n"},
		{"2to0 delete at EOF", lf, 3, -1, "", "a\nb\n"},
		{"2to0 delete first lines", lf, 1, 2, "", "c\nd\n"},
		{"a lone newline is one blank line", lf, 2, 3, "\n", "a\n\nd\n"},
		// First line.
		{"first line bare", lf, 1, 1, "A", "A\nb\nc\nd\n"},
		// Last line of a file WITH a final newline keeps it.
		{"last line bare", lf, 4, 4, "D", "a\nb\nc\nD\n"},
		{"last line terminated", lf, 4, 4, "D\n", "a\nb\nc\nD\n"},
		{"last line to 2 bare", lf, 4, -1, "D1\nD2", "a\nb\nc\nD1\nD2\n"},
		// A file with NO final newline: an interior line is still terminated...
		{"no final newline interior bare", noFinal, 2, 2, "B", "a\nB\nc"},
		{"no final newline interior to 2", noFinal, 1, 1, "A1\nA2", "A1\nA2\nb\nc"},
		// ...but a range to EOF preserves the missing final newline exactly,
		{"no final newline last bare", noFinal, 3, 3, "C", "a\nb\nC"},
		{"no final newline to EOF bare", noFinal, 2, -1, "X\nY", "a\nX\nY"},
		{"no final newline end capped", noFinal, 3, 99, "C", "a\nb\nC"},
		// and an explicit trailing newline from the caller is honoured.
		{"no final newline last terminated", noFinal, 3, 3, "C\n", "a\nb\nC\n"},
		// CRLF: the added terminator is the replaced line's own \r\n.
		{"crlf 1to1 bare", crlf, 2, 2, "B", "a\r\nB\r\nc\r\nd\r\n"},
		{"crlf 1to2 bare", crlf, 2, 2, "B1\r\nB2", "a\r\nB1\r\nB2\r\nc\r\nd\r\n"},
		{"crlf last line bare", crlf, 4, -1, "D", "a\r\nb\r\nc\r\nD\r\n"},
		{"crlf no final newline last bare", "a\r\nb", 2, 2, "B", "a\r\nB"},
		// Empty file: there is no final newline to preserve.
		{"empty file bare", "", 1, 0, "x", "x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := applyRangeEdit(tc.content, tc.start, tc.end, tc.newStr)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("applyRangeEdit(%q, %d, %d, %q)\n got  %q\n want %q",
					tc.content, tc.start, tc.end, tc.newStr, got, tc.want)
			}
		})
	}
}

// TestApplyRangeEdit_AppendWholeLines covers append mode (start_line -1) under
// the same rule: the file keeps its final-newline state, and the separator
// inserted before an unterminated last line uses the file's line ending.
func TestApplyRangeEdit_AppendWholeLines(t *testing.T) {
	cases := []struct {
		name, content, newStr, want string
	}{
		{"final newline, bare", "a\n", "b", "a\nb\n"},
		{"final newline, terminated", "a\n", "b\n", "a\nb\n"},
		{"final newline, two lines bare", "a\n", "b\nc", "a\nb\nc\n"},
		{"no final newline, bare", "a", "b", "a\nb"},
		{"no final newline, terminated", "a", "b\n", "a\nb\n"},
		{"crlf final newline, bare", "a\r\n", "b", "a\r\nb\r\n"},
		{"crlf no final newline, bare", "a\r\nb", "c", "a\r\nb\r\nc"},
		{"empty file", "", "b", "b"},
		// Appending nothing is a no-op: it must not add a final newline either.
		{"empty new_string", "a\n", "", "a\n"},
		{"empty new_string, no final newline", "a", "", "a"},
		{"empty new_string, crlf no final newline", "a\r\nb", "", "a\r\nb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := applyRangeEdit(tc.content, -1, 0, tc.newStr)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("append(%q, %q)\n got  %q\n want %q", tc.content, tc.newStr, got, tc.want)
			}
		})
	}
}

// TestEditFile_RangeEdit_IssueRepro reproduces #543 end to end: replacing one
// struct field with two, new_string without a trailing newline, glued the next
// field onto the second line and broke the build.
func TestEditFile_RangeEdit_IssueRepro(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deps.go")
	const src = "type deps struct {\n\tlspDiagModeFn func() string\n\tpurposeFn func(purpose string)\n}\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := callEditFile(t, map[string]any{
		"file_path": path,
		"edits": []map[string]any{{
			"start_line": 2, "end_line": 2,
			"new_string": "\tlspDiagModeFn func() string\n\tlspGoWorkFn func() string",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	const want = "type deps struct {\n\tlspDiagModeFn func() string\n\tlspGoWorkFn func() string\n\tpurposeFn func(purpose string)\n}\n"
	if got, _ := os.ReadFile(path); string(got) != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// TestEditFile_RangeEdit_BatchAndCRLF runs several range edits in one call —
// each sees the content the previous one produced, so a glued line would also
// shift every later edit — over an LF and a CRLF file, in both the atomic and
// the apply_partial path.
func TestEditFile_RangeEdit_BatchAndCRLF(t *testing.T) {
	edits := []map[string]any{
		{"start_line": 1, "end_line": 1, "new_string": "one"},
		{"start_line": 3, "end_line": 4, "new_string": "three\nfour-a\nfour-b"},
		{"start_line": 6, "end_line": 6, "new_string": ""},
		{"start_line": -1, "new_string": "six"},
	}
	cases := []struct {
		name, content, want string
	}{
		{"lf", "1\n2\n3\n4\n5\n", "one\n2\nthree\nfour-a\nfour-b\nsix\n"},
		{"crlf", "1\r\n2\r\n3\r\n4\r\n5\r\n", "one\r\n2\r\nthree\r\nfour-a\r\nfour-b\r\nsix\r\n"},
	}
	for _, tc := range cases {
		for _, partial := range []bool{false, true} {
			name := tc.name
			if partial {
				name += "/apply_partial"
			}
			t.Run(name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "f.txt")
				if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
					t.Fatal(err)
				}
				args := map[string]any{"file_path": path, "edits": edits}
				if partial {
					args["apply_partial"] = true
				}
				if _, err := callEditFile(t, args); err != nil {
					t.Fatal(err)
				}
				if got, _ := os.ReadFile(path); string(got) != tc.want {
					t.Errorf("got  %q\nwant %q", got, tc.want)
				}
			})
		}
	}
}

// viewRows splits a ranged read_file reply into its rows: the file line number
// from the gutter and the line text after the "<n>\t" prefix. #562 ends every
// row with its own newline, so a blank row at either edge is still a row.
func viewRows(t *testing.T, view string) (nums []int, texts []string) {
	t.Helper()
	_, body, ok := strings.Cut(view, "\n\n") // header block, blank line, then rows
	if !ok || !strings.HasSuffix(body, "\n") {
		t.Fatalf("read_file window is not a header and terminated rows: %q", view)
	}
	for _, row := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		num, text, found := strings.Cut(row, "\t")
		n, err := strconv.Atoi(strings.TrimSpace(num))
		if !found || err != nil {
			t.Fatalf("row %q has no \"<n>\\t\" gutter", row)
		}
		nums, texts = append(nums, n), append(texts, text)
	}
	return nums, texts
}

// TestEditFile_RangeEdit_FromReadFileWindow pins how #543 (range edits replace
// whole lines) and #562 (read_file terminates every row of a window and keeps
// its blank edges) fit together: the text an agent copies out of a ranged read,
// edited and sent back over the same line numbers, changes exactly the one row
// it touched. The edited row is the first or the last of the window, so a blank
// row at that edge is the one replaced, and the replacement is sent both with
// and without its trailing newline. read_file strips "\r", so the CRLF cases
// also prove the file's own line endings survive a round trip through the view.
//
// A bare new_string cannot spell a window whose last row is blank and unedited:
// "x\n" is one line, so only the terminated spelling is sent there.
func TestEditFile_RangeEdit_FromReadFileWindow(t *testing.T) {
	cases := []struct {
		name, file string
		start, end int
		blankLast  bool // the window's last row is blank
	}{
		{"interior window", "a\nb\nc\nd\ne\n", 2, 4, false},
		{"blank first and last rows", "a\n\nc\nd\n\ne\n", 2, 5, true},
		{"only blank rows", "a\n\n\nd\n", 2, 3, true},
		{"blank first line of the file", "\nb\nc\n", 1, 2, false},
		{"through the last line", "a\nb\nc\n", 2, 3, false},
		{"crlf interior", "a\r\nb\r\nc\r\nd\r\n", 2, 3, false},
		{"crlf blank edges", "a\r\n\r\nc\r\n\r\ne\r\n", 2, 4, true},
	}
	for _, tc := range cases {
		for _, edited := range []string{"first", "last"} {
			for _, terminated := range []bool{true, false} {
				if !terminated && edited == "first" && tc.blankLast {
					continue
				}
				name := tc.name + "/" + edited + " row/bare"
				if terminated {
					name = tc.name + "/" + edited + " row/terminated"
				}
				t.Run(name, func(t *testing.T) {
					path := filepath.Join(t.TempDir(), "f.txt")
					if err := os.WriteFile(path, []byte(tc.file), 0o644); err != nil {
						t.Fatal(err)
					}
					view, err := callReadFile(t, map[string]any{"file_path": path, "start_line": tc.start, "end_line": tc.end})
					if err != nil {
						t.Fatal(err)
					}
					nums, texts := viewRows(t, view)
					if len(nums) != tc.end-tc.start+1 || nums[0] != tc.start || nums[len(nums)-1] != tc.end {
						t.Fatalf("the view must number its rows %d..%d, got %v", tc.start, tc.end, nums)
					}
					row := 0
					if edited == "last" {
						row = len(nums) - 1
					}
					texts[row] = "CHANGED"
					newStr := strings.Join(texts, "\n")
					if terminated {
						newStr += "\n"
					}
					if _, err := callEditFile(t, map[string]any{
						"file_path": path,
						"edits":     []map[string]any{{"start_line": tc.start, "end_line": tc.end, "new_string": newStr}},
					}); err != nil {
						t.Fatal(err)
					}

					// Oracle: the original file with exactly that one line replaced,
					// keeping the line ending it had.
					lines := strings.SplitAfter(tc.file, "\n")
					eol := "\n"
					if strings.HasSuffix(lines[nums[row]-1], "\r\n") {
						eol = "\r\n"
					}
					lines[nums[row]-1] = "CHANGED" + eol
					want := strings.Join(lines, "")
					if got, _ := os.ReadFile(path); string(got) != want {
						t.Errorf("read_file view %q, new_string %q\n got  %q\n want %q", view, newStr, got, want)
					}
				})
			}
		}
	}
}
