package cli

import (
	"errors"
	"log/slog"
	"testing"
)

func TestAvailableCommandNameWidthUsesLongestAvailableCommand(t *testing.T) {
	got := availableCommandNameWidth(setupCmd)
	want := len("claude-desktop") + 1
	if got != want {
		t.Fatalf("availableCommandNameWidth(setupCmd) = %d, want %d", got, want)
	}
}

func TestSetupLogging_InvalidLevelReturnsError(t *testing.T) {
	if err := setupLogging("nonsense", "text"); err == nil {
		t.Fatal("expected error for invalid log level")
	}
}

func TestSetupLogging_ValidLevelAndFormat(t *testing.T) {
	for _, tt := range []struct{ level, format string }{
		{"debug", "text"},
		{"info", "text"},
		{"warn", "json"},
		{"error", "json"},
	} {
		if err := setupLogging(tt.level, tt.format); err != nil {
			t.Errorf("setupLogging(%q, %q) returned error: %v", tt.level, tt.format, err)
		}
	}
	// Restore a sane default after the test.
	_ = setupLogging("info", "text")
}

// TestSetupLogging_JSONHandler and TestSetupLogging_TextHandler mutate the
// process-global slog.Default(). They must NOT be marked t.Parallel() and must
// restore the default on exit. This mirrors the daemon's own usage: setupLogging
// is called once at daemon startup, before any connection goroutines are spawned.
func TestSetupLogging_JSONHandler(t *testing.T) {
	t.Cleanup(func() { _ = setupLogging("info", "text") })
	if err := setupLogging("info", "json"); err != nil {
		t.Fatalf("setupLogging(info, json): %v", err)
	}
	_, ok := slog.Default().Handler().(*slog.JSONHandler)
	if !ok {
		t.Errorf("expected *slog.JSONHandler after setupLogging with format=json, got %T", slog.Default().Handler())
	}
}

func TestSetupLogging_TextHandler(t *testing.T) {
	t.Cleanup(func() { _ = setupLogging("info", "text") })
	if err := setupLogging("info", "text"); err != nil {
		t.Fatalf("setupLogging(info, text): %v", err)
	}
	_, ok := slog.Default().Handler().(*slog.TextHandler)
	if !ok {
		t.Errorf("expected *slog.TextHandler after setupLogging with format=text, got %T", slog.Default().Handler())
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
