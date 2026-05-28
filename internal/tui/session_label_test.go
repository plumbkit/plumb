package tui

import (
	"testing"

	"github.com/golimpio/plumb/internal/session"
)

func TestSessionLangLabelPrefersDetectedLanguage(t *testing.T) {
	got := sessionLangLabel(session.Info{
		Language:         "none",
		DetectedLanguage: "java",
	})
	if got != "java" {
		t.Fatalf("sessionLangLabel = %q, want java", got)
	}
}

func TestSessionLangLabelUnknownLanguage(t *testing.T) {
	got := sessionLangLabel(session.Info{Language: "none"})
	if got != "?" {
		t.Fatalf("sessionLangLabel = %q, want ?", got)
	}
}
