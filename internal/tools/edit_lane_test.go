package tools

import (
	"strings"
	"testing"
)

func TestClientHasNativeEditConflict(t *testing.T) {
	cases := []struct {
		name   string
		client func() string
		want   bool
	}{
		{"claude-code exact", func() string { return "claude-code" }, true},
		{"claude-code versioned", func() string { return "claude-code/1.2.3" }, true},
		{"claude-code mixed case", func() string { return "Claude-Code" }, true},
		{"claude desktop (no native edit)", func() string { return "claude-ai" }, false},
		{"unrelated client", func() string { return "vscode" }, false},
		{"prefix-only false positive", func() string { return "claude-codegen" }, false},
		{"empty", func() string { return "" }, false},
		{"nil accessor", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := clientHasNativeEditConflict(c.client); got != c.want {
				t.Errorf("clientHasNativeEditConflict = %v, want %v", got, c.want)
			}
		})
	}
}

func TestNativeEditReadHint_Actionable(t *testing.T) {
	const mtime = "2026-06-02T22:03:58.123456789+10:00"
	hint := nativeEditReadHint(mtime)
	for _, want := range []string{"edit_file", "native Edit", "expected_mtime", mtime} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint missing %q: %q", want, hint)
		}
	}
	// Must be a single comment line so it reads as part of the read_file header
	// block rather than as file content.
	if !strings.HasPrefix(hint, "# ") {
		t.Errorf("hint must be a comment line, got %q", hint)
	}
	if strings.Count(hint, "\n") != 1 || !strings.HasSuffix(hint, "\n") {
		t.Errorf("hint must be exactly one line terminated by \\n, got %q", hint)
	}
}

// TestNativeEditLaneWarning_LoadBearingPhrases pins the warning text to the
// phrases that make it useful: the anti-pattern, the two exact harness errors,
// and the correct plumb tools. If the warning is reworded and loses one of
// these, this fails — the text is the whole point of the fix.
func TestNativeEditLaneWarning_LoadBearingPhrases(t *testing.T) {
	for _, want := range []string{
		"Edit lane",
		"read_file",
		"edit_file",
		"native",
		"File has not been read yet",
		"File has been modified since read",
		"expected_mtime",
	} {
		if !strings.Contains(nativeEditLaneWarning, want) {
			t.Errorf("nativeEditLaneWarning missing %q", want)
		}
	}
}
