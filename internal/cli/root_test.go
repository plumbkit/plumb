package cli

import (
	"errors"
	"testing"
)

func TestAvailableCommandNameWidthUsesLongestAvailableCommand(t *testing.T) {
	got := availableCommandNameWidth(setupCmd)
	want := len("claude-desktop") + 1
	if got != want {
		t.Fatalf("availableCommandNameWidth(setupCmd) = %d, want %d", got, want)
	}
}

func TestDiagnosticSuggestionsForRecoverableErrors(t *testing.T) {
	got := diagnosticSuggestions(errors.New("unknown command \"wat\" for \"plumb\""))
	if len(got) != 1 || got[0] != "plumb --help" {
		t.Fatalf("diagnosticSuggestions unknown command = %#v", got)
	}

	got = diagnosticSuggestions(errors.New("no workspace found"))
	if len(got) != 2 || got[0] != "plumb init" {
		t.Fatalf("diagnosticSuggestions no workspace = %#v", got)
	}
}
