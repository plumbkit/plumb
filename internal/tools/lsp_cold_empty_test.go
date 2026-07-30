package tools

import (
	"strings"
	"testing"
	"time"
)

// A cold language server does not only FAIL — sourcekit-lsp and jdtls answer
// with an empty result until indexing completes. The error paths were already
// rewritten into warming advisories; these two helpers carry the other half,
// where a confident negative ("no references found") or a clean diagnostics
// report is the most dangerous answer plumb can give, because an agent acts on
// it (deletes the symbol / believes the build is fine). The per-tool wiring is
// pinned in lsp_cold_empty_tools_test.go.

func TestColdLSPEmptyNote_OnlyWhenWarming(t *testing.T) {
	note := coldLSPEmptyNote(func(string) (bool, time.Duration) { return true, 4 * time.Second }, "file:///p/a.go")
	if note == "" {
		t.Fatal("a warming server must produce the not-evidence-of-absence caveat")
	}
	for _, want := range []string{"still warming", "(~4s elapsed)", "NOT evidence of absence", "daemon_info"} {
		if !strings.Contains(note, want) {
			t.Errorf("caveat missing %q: %q", want, note)
		}
	}
	if note := coldLSPEmptyNote(func(string) (bool, time.Duration) { return false, 0 }, "file:///p/a.go"); note != "" {
		t.Errorf("a ready server must add no caveat, got %q", note)
	}
	if note := coldLSPEmptyNote(nil, "file:///p/a.go"); note != "" {
		t.Errorf("an unwired probe must add no caveat, got %q", note)
	}
}

func TestColdLSPIncompleteNote_OnlyWhenWarming(t *testing.T) {
	note := coldLSPIncompleteNote(func(string) (bool, time.Duration) { return true, 2 * time.Second }, "")
	for _, want := range []string{"still warming", "INCOMPLETE", "NOT proof the code compiles"} {
		if !strings.Contains(note, want) {
			t.Errorf("caveat missing %q: %q", want, note)
		}
	}
	if note := coldLSPIncompleteNote(func(string) (bool, time.Duration) { return false, 0 }, ""); note != "" {
		t.Errorf("a ready server must add no caveat, got %q", note)
	}
	if note := coldLSPIncompleteNote(nil, ""); note != "" {
		t.Errorf("an unwired probe must add no caveat, got %q", note)
	}
}
