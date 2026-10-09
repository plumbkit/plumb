package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// git_window_test.go covers PLAN-454's read-tier output window: the pure
// semantics of the slice/pattern itself, and the end-to-end property that
// matters most — a window deep inside a blob LARGER than the response cap is
// still answered, because the window is applied to the child's full output
// before formatGitOutput's caps.

func intPtr(n int) *int { return &n }

func TestWindowGitArgs_Refusals(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{
			"use_regex needs a pattern",
			map[string]any{"subcommand": "status", "use_regex": true},
			"use_regex and case_sensitive qualify pattern",
		},
		{
			"case_sensitive needs a pattern",
			map[string]any{"subcommand": "status", "case_sensitive": false},
			"use_regex and case_sensitive qualify pattern",
		},
		{
			"start_line is 1-based",
			map[string]any{"subcommand": "log", "start_line": 0},
			"start_line must be >= 1",
		},
		{
			"end_line is 1-based",
			map[string]any{"subcommand": "log", "end_line": 0},
			"end_line must be >= 1",
		},
		{
			"end before start",
			map[string]any{"subcommand": "log", "start_line": 9, "end_line": 3},
			"end_line (3) is before start_line (9)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			a, err := parseGitArgs(raw)
			if err != nil {
				t.Fatalf("parseGitArgs: %v", err)
			}
			_, err = windowGitArgs(a)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("windowGitArgs = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestApplyGitWindow_Semantics(t *testing.T) {
	const out = "l1\nl2\nl3\nl4\nl5\n"
	cases := []struct {
		name   string
		window gitWindow
		want   string
	}{
		{"range", gitWindow{Start: intPtr(2), End: intPtr(4)}, "l2\nl3\nl4\n… (lines 2–4 of 5)"},
		{"open start", gitWindow{End: intPtr(2)}, "l1\nl2\n… (lines 1–2 of 5)"},
		{"open end", gitWindow{Start: intPtr(4)}, "l4\nl5\n… (lines 4–5 of 5)"},
		{"the whole output says so", gitWindow{Start: intPtr(1), End: intPtr(5)}, out + "… (all 5 lines)"},
		{"an end past EOF clamps", gitWindow{Start: intPtr(4), End: intPtr(99)}, "l4\nl5\n… (lines 4–5 of 5)"},
		{
			"a start past EOF names the range",
			gitWindow{Start: intPtr(9)},
			"(no lines in range 9–EOF; the output has 5 lines)",
		},
		{
			"a start past EOF keeps a given end",
			gitWindow{Start: intPtr(9), End: intPtr(12)},
			"(no lines in range 9–12; the output has 5 lines)",
		},
		{"pattern", gitWindow{Pattern: "l3"}, "3\tl3\n… (1 matching line(s), 5 scanned)"},
		{
			"pattern narrowed by a range",
			gitWindow{Start: intPtr(4), Pattern: "l"},
			"4\tl4\n5\tl5\n… (2 matching line(s), 2 scanned)",
		},
		{"no match", gitWindow{Pattern: "nope"}, "(no lines matched \"nope\" in lines 1–5 of 5)"},
		{"no window returns the output whole", gitWindow{}, out},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := applyGitWindow(out, tc.window)
			if err != nil {
				t.Fatalf("applyGitWindow: %v", err)
			}
			if got != tc.want {
				t.Errorf("applyGitWindow:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestGit_ShowWindowBeyondTheResponseCap is the regression for the friction the
// card was filed from: a 102 KB blob answered a 60-line question with the whole
// file. A window near the END of a blob larger than maxGitBytes only works if
// the window is applied before the byte cap — otherwise the lines the caller
// asked for were already discarded.
func TestGit_ShowWindowBeyondTheResponseCap(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)

	var sb strings.Builder
	for i := 1; i <= 3000; i++ {
		fmt.Fprintf(&sb, "line %04d %s\n", i, strings.Repeat("x", 48))
	}
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "big.txt")
	gitRun(t, dir, "commit", "-m", "big blob")
	if got := len(sb.String()); got <= maxGitBytes {
		t.Fatalf("fixture is %d bytes, which does not exceed the %d-byte response cap", got, maxGitBytes)
	}

	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{AllowWrites: true} })
	out, err := callGit(t, tool, map[string]any{
		"subcommand": "show", "args": []string{"HEAD:big.txt"}, "repo": dir,
		"start_line": 2900, "end_line": 2902,
	})
	if err != nil {
		t.Fatalf("show with a line range: %v", err)
	}
	for _, want := range []string{"line 2900 ", "line 2901 ", "line 2902 ", "(lines 2900–2902 of 3000)"} {
		if !strings.Contains(out, want) {
			t.Errorf("windowed show is missing %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "truncated at 100 KiB") {
		t.Errorf("the window was applied after the response cap, so the requested lines were already gone:\n%s", out)
	}
	if n := strings.Count(out, "\n"); n != 3 {
		t.Errorf("windowed show returned %d newline-terminated lines, want 3:\n%s", n, out)
	}

	// A pattern window is the same question asked differently, and must reach the
	// same lines.
	out, err = callGit(t, tool, map[string]any{
		"subcommand": "show", "args": []string{"HEAD:big.txt"}, "repo": dir,
		"pattern": "line 0042 ",
	})
	if err != nil {
		t.Fatalf("show with a pattern: %v", err)
	}
	if want := "42\tline 0042 "; !strings.Contains(out, want) {
		t.Errorf("pattern window is missing %q (line-numbered like read_file's search):\n%s", want, out)
	}
	if want := "(1 matching line(s), 3000 scanned)"; !strings.Contains(out, want) {
		t.Errorf("pattern window is missing the count note %q:\n%s", want, out)
	}
}

// TestGit_WindowLiteralPatternHint pins the shared literal/regex hint reaching
// the git window too: the pattern below carries regex syntax and was matched
// literally, which is exactly the silent misreading read_file's hint exists for.
func TestGit_WindowLiteralPatternHint(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{AllowWrites: true} })

	out, err := callGit(t, tool, map[string]any{
		"subcommand": "show", "args": []string{"HEAD:init.txt"}, "repo": dir,
		"pattern": `init\d`,
	})
	if err != nil {
		t.Fatalf("show with a literal pattern: %v", err)
	}
	if !strings.Contains(out, "use_regex is false") {
		t.Errorf("a literal pattern carrying regex syntax must carry the hint; got:\n%s", out)
	}

	out, err = callGit(t, tool, map[string]any{
		"subcommand": "show", "args": []string{"HEAD:init.txt"}, "repo": dir,
		"pattern": `^init$`, "use_regex": true,
	})
	if err != nil {
		t.Fatalf("show with a regex pattern: %v", err)
	}
	if !strings.Contains(out, "\tinit") {
		t.Errorf("regex pattern did not match the committed line; got:\n%s", out)
	}
}

// TestGit_WindowIsReadTierOnly keeps the window honest about its scope: it
// slices what a read command printed, so a write carrying it is caller error.
func TestGit_WindowIsReadTierOnly(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{AllowWrites: true} })

	_, err := callGit(t, tool, map[string]any{
		"subcommand": "add", "files": []string{filepath.Join(dir, "init.txt")}, "repo": dir,
		"start_line": 1,
	})
	if err == nil || !strings.Contains(err.Error(), "window a read command's output") {
		t.Fatalf("a window on a write subcommand = %v, want a refusal naming the read tier", err)
	}
}

// TestGitSchema_AdvertisesTheWindow pins the schema half of the contract: a
// parameter no client can see is a parameter no client can use.
func TestGitSchema_AdvertisesTheWindow(t *testing.T) {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(gitSchema, &schema); err != nil {
		t.Fatalf("gitSchema is not valid JSON: %v", err)
	}
	for _, name := range []string{"start_line", "end_line", "pattern", "use_regex", "case_sensitive"} {
		if _, ok := schema.Properties[name]; !ok {
			t.Errorf("git schema does not advertise %q", name)
		}
	}
}

// gitRun runs a git command in dir for a fixture, failing the test on error.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
