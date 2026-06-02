package tools

import (
	"strings"
	"testing"
)

func TestLineOffsets_Empty(t *testing.T) {
	if lineOffsets("") != nil {
		t.Fatal("expected nil for empty string")
	}
}

func TestLineOffsets_SingleLine(t *testing.T) {
	got := lineOffsets("hello\n")
	if len(got) != 1 || got[0] != 0 {
		t.Fatalf("want [0], got %v", got)
	}
}

func TestLineOffsets_MultiLine(t *testing.T) {
	// "a\nb\nc\n" → 3 lines starting at bytes 0, 2, 4
	got := lineOffsets("a\nb\nc\n")
	want := []int{0, 2, 4}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("[%d] want %d, got %d", i, w, got[i])
		}
	}
}

func TestLineOffsets_NoTrailingNewline(t *testing.T) {
	got := lineOffsets("a\nb")
	want := []int{0, 2}
	if len(got) != 2 || got[0] != 0 || got[1] != 2 {
		t.Fatalf("want %v, got %v", want, got)
	}
}

func TestApplyRangeEdit_DeleteMiddle(t *testing.T) {
	content := "line1\nline2\nline3\nline4\n"
	result, err := applyRangeEdit(content, 2, 3, "")
	if err != nil {
		t.Fatal(err)
	}
	want := "line1\nline4\n"
	if result != want {
		t.Fatalf("want %q, got %q", want, result)
	}
}

func TestApplyRangeEdit_ReplaceLines(t *testing.T) {
	content := "line1\nline2\nline3\n"
	result, err := applyRangeEdit(content, 2, 2, "replaced\n")
	if err != nil {
		t.Fatal(err)
	}
	want := "line1\nreplaced\nline3\n"
	if result != want {
		t.Fatalf("want %q, got %q", want, result)
	}
}

func TestApplyRangeEdit_AppendEOF_WithTrailingNewline(t *testing.T) {
	content := "line1\nline2\n"
	result, err := applyRangeEdit(content, -1, 0, "line3\n")
	if err != nil {
		t.Fatal(err)
	}
	want := "line1\nline2\nline3\n"
	if result != want {
		t.Fatalf("want %q, got %q", want, result)
	}
}

func TestApplyRangeEdit_AppendEOF_NoTrailingNewline(t *testing.T) {
	content := "line1\nline2"
	result, err := applyRangeEdit(content, -1, 0, "line3\n")
	if err != nil {
		t.Fatal(err)
	}
	want := "line1\nline2\nline3\n"
	if result != want {
		t.Fatalf("want %q, got %q", want, result)
	}
}

func TestApplyRangeEdit_DeleteToEOF(t *testing.T) {
	content := "line1\nline2\nline3\nline4\n"
	result, err := applyRangeEdit(content, 3, -1, "")
	if err != nil {
		t.Fatal(err)
	}
	want := "line1\nline2\n"
	if result != want {
		t.Fatalf("want %q, got %q", want, result)
	}
}

func TestApplyRangeEdit_SingleLine(t *testing.T) {
	// end_line == 0 defaults to start_line
	content := "a\nb\nc\n"
	result, err := applyRangeEdit(content, 2, 0, "B\n")
	if err != nil {
		t.Fatal(err)
	}
	want := "a\nB\nc\n"
	if result != want {
		t.Fatalf("want %q, got %q", want, result)
	}
}

func TestApplyRangeEdit_OutOfRange(t *testing.T) {
	content := "line1\nline2\n"
	if _, err := applyRangeEdit(content, 5, 0, "x"); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("expected out-of-range error, got: %v", err)
	}
}

func TestApplyRangeEdit_EndBeforeStart(t *testing.T) {
	content := "line1\nline2\nline3\n"
	if _, err := applyRangeEdit(content, 3, 2, "x"); err == nil {
		t.Fatal("expected error when end_line < start_line")
	}
}

func TestApplyRangeEdit_EndCapToFileLength(t *testing.T) {
	// end_line beyond total lines is silently capped to the last line
	content := "line1\nline2\n"
	result, err := applyRangeEdit(content, 2, 9999, "new\n")
	if err != nil {
		t.Fatal(err)
	}
	want := "line1\nnew\n"
	if result != want {
		t.Fatalf("want %q, got %q", want, result)
	}
}

func TestApplyRangeEdit_DeleteFirstLine(t *testing.T) {
	content := "line1\nline2\nline3\n"
	result, err := applyRangeEdit(content, 1, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	want := "line2\nline3\n"
	if result != want {
		t.Fatalf("want %q, got %q", want, result)
	}
}

func TestApplyRangeEdit_EmptyFile(t *testing.T) {
	result, err := applyRangeEdit("", 1, 0, "hello\n")
	if err != nil {
		t.Fatal(err)
	}
	if result != "hello\n" {
		t.Fatalf("want %q, got %q", "hello\n", result)
	}
}
