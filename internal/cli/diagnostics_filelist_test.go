package cli

import (
	"path/filepath"
	"slices"
	"testing"
)

// TestParseFileList_SkipsFindFilesProse is the guard for the one thing this
// parser can get wrong without anyone noticing: `plumb diagnostics` feeds
// find_files' raw text output back in as a file list, so every non-path line
// find_files can emit has to be recognised as prose. A missed one does not
// error — it becomes a filename, and the scan spends an LSP round trip on
// "find_files under /x timed out before any matches were found (budget 30s …)"
// before reporting it as a file with no diagnostics.
//
// The timeout sentence is matched on "timed out" rather than on its opening,
// which is the wording most likely to be rewritten later.
func TestParseFileList_SkipsFindFilesProse(t *testing.T) {
	const cwd = "/ws"
	output := "main.go\n" +
		"note: /ws/main.go is a file — listing its parent directory /ws.\n" +
		"internal/tools/walk.go\n" +
		"find_files under /ws timed out before any matches were found (budget 30s — narrow with path or max_depth).\n" +
		"No files found matching \"*.zz\".\n" +
		"(truncated at 2 results — use a more specific pattern or set max_depth)\n" +
		"12 result(s) (partial — walk timed out after 30s; narrow with path or max_depth)\n" +
		"\n" +
		"3 result(s)\n"

	want := []string{
		filepath.Join(cwd, "main.go"),
		filepath.Join(cwd, "internal/tools/walk.go"),
	}
	if got := parseFileList(output, cwd); !slices.Equal(got, want) {
		t.Errorf("parseFileList = %v, want %v", got, want)
	}
}
