package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStripLineGutter(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"two sequential lines", "2\tbeta\n3\tgamma", "beta\ngamma", true},
		{"trailing newline preserved", "2\tbeta\n3\tgamma\n", "beta\ngamma\n", true},
		{"right-aligned padding", " 9\tx\n10\ty", "x\ny", true},
		{"single line never stripped", "2\tbeta", "", false},
		{"non-sequential numbers", "2\tbeta\n5\tgamma", "", false},
		{"a line without gutter", "2\tbeta\ngamma", "", false},
		{"no tab after digits", "2 beta\n3 gamma", "", false},
		{"empty string", "", "", false},
		{"guttered empty line", "7\t\n8\tfoo", "\nfoo", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := stripLineGutter(c.in)
			if ok != c.ok {
				t.Fatalf("stripLineGutter(%q) ok = %v, want %v", c.in, ok, c.ok)
			}
			if ok && got != c.want {
				t.Fatalf("stripLineGutter(%q) = %q, want %q", c.in, got, c.want)
			}
			if !ok && got != c.in {
				t.Fatalf("a refused strip must return the input unchanged, got %q", got)
			}
		})
	}
}

func TestEditFile_GutterForgiveness_MultiLine(t *testing.T) {
	// An old_string pasted verbatim from guttered read_file output (multi-line,
	// sequential numbers) is stripped automatically; the edit applies in one
	// round-trip and the response carries the teaching note.
	path := filepath.Join(t.TempDir(), "f.go")
	_ = os.WriteFile(path, []byte("alpha\nbeta\ngamma\ndelta\n"), 0o644)

	out, err := callEditFile(t, map[string]any{
		"file_path": path,
		"edits": []map[string]string{{
			"old_string": "2\tbeta\n3\tgamma",
			"new_string": "BETA\nGAMMA",
		}},
	})
	if err != nil {
		t.Fatalf("guttered multi-line edit should be forgiven, got: %v", err)
	}
	if !strings.Contains(out, "gutter") {
		t.Errorf("expected the gutter-stripped note in the response:\n%s", out)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "alpha\nBETA\nGAMMA\ndelta\n" {
		t.Errorf("unexpected file content: %q", data)
	}
}

func TestEditFile_GutterForgiveness_NewStringAlsoStripped(t *testing.T) {
	// A verbatim guttered paste usually gutters new_string too — it must be
	// stripped as well, so gutter text never lands in the file.
	path := filepath.Join(t.TempDir(), "f.go")
	_ = os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"file_path": path,
		"edits": []map[string]string{{
			"old_string": "1\talpha\n2\tbeta",
			"new_string": "1\tALPHA\n2\tBETA",
		}},
	})
	if err != nil {
		t.Fatalf("edit failed: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "ALPHA\nBETA\ngamma\n" {
		t.Errorf("guttered new_string must be stripped too, got: %q", data)
	}
}

func TestEditFile_SingleLineGutter_HintNotAutoFix(t *testing.T) {
	// A single guttered line is plausible real content (a TSV row), so it is
	// never auto-stripped — the not-found error carries the hint instead.
	path := filepath.Join(t.TempDir(), "f.go")
	original := "alpha\nbeta\ngamma\n"
	_ = os.WriteFile(path, []byte(original), 0o644)

	_, err := callEditFile(t, map[string]any{
		"file_path": path,
		"edits": []map[string]string{{
			"old_string": "2\tbeta",
			"new_string": "BETA",
		}},
	})
	if err == nil {
		t.Fatal("single-line guttered old_string must not be auto-fixed")
	}
	if !strings.Contains(err.Error(), "gutter") {
		t.Errorf("expected the gutter hint in the error, got: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != original {
		t.Errorf("file must be unmodified, got: %q", data)
	}
}

func TestEditFile_LiteralTabDigitContentUnaffected(t *testing.T) {
	// Literal-first: content that genuinely contains digit+tab lines (TSV)
	// matches as-is — forgiveness never fires, no note is emitted.
	path := filepath.Join(t.TempDir(), "data.tsv")
	_ = os.WriteFile(path, []byte("1\ta\n2\tb\n3\tc\n"), 0o644)

	out, err := callEditFile(t, map[string]any{
		"file_path": path,
		"edits": []map[string]string{{
			"old_string": "1\ta\n2\tb",
			"new_string": "1\tA\n2\tB",
		}},
	})
	if err != nil {
		t.Fatalf("literal TSV edit failed: %v", err)
	}
	if strings.Contains(out, "gutter") {
		t.Errorf("literal match must not trigger gutter forgiveness:\n%s", out)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "1\tA\n2\tB\n3\tc\n" {
		t.Errorf("unexpected file content: %q", data)
	}
}

func TestEditFile_GutterForgiveness_PartialMode(t *testing.T) {
	// apply_partial uses the same matcher: a guttered edit applies and its
	// result line carries the note.
	path := filepath.Join(t.TempDir(), "f.go")
	_ = os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o644)

	out, err := callEditFile(t, map[string]any{
		"file_path":     path,
		"apply_partial": true,
		"edits": []map[string]any{{
			"old_string": "2\tbeta\n3\tgamma",
			"new_string": "BETA\nGAMMA",
		}},
	})
	if err != nil {
		t.Fatalf("partial-mode guttered edit failed: %v", err)
	}
	if !strings.Contains(out, "applied") || !strings.Contains(out, "gutter") {
		t.Errorf("expected applied result with gutter note:\n%s", out)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "alpha\nBETA\nGAMMA\n" {
		t.Errorf("unexpected file content: %q", data)
	}
}

func TestEditFile_GutterForgiveness_AmbiguousStrippedFormNotApplied(t *testing.T) {
	// If the stripped form would match more than once, forgiveness must not
	// fire (without replace_all) — the original not-found error surfaces and
	// the file stays untouched.
	path := filepath.Join(t.TempDir(), "g.go")
	original := "a\nb\na\nb\n"
	_ = os.WriteFile(path, []byte(original), 0o644)

	_, err := callEditFile(t, map[string]any{
		"file_path": path,
		"edits": []map[string]string{{
			"old_string": "1\ta\n2\tb", // stripped "a\nb" appears twice → refuse
			"new_string": "x",
		}},
	})
	if err == nil {
		t.Fatal("ambiguous stripped form must not be applied")
	}
	data, _ := os.ReadFile(path)
	if string(data) != original {
		t.Errorf("file must be unmodified on refusal, got: %q", data)
	}
}
