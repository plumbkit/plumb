package tools

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"

	"github.com/golimpio/plumb/internal/session"
)

func TestRenameSession_PreservesCase(t *testing.T) {
	var got string
	tool := NewRenameSession(func(name string) (string, error) {
		got = name
		return "build-fix", nil
	})

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"build-fix"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got != "build-fix" {
		t.Fatalf("rename callback got %q, want build-fix", got)
	}
	if out != "session renamed to build-fix" {
		t.Fatalf("output = %q, want 'session renamed to build-fix'", out)
	}
}

func TestRenameSession_MixedCase(t *testing.T) {
	var got string
	tool := NewRenameSession(func(name string) (string, error) {
		got = name
		return "Build-Fix", nil
	})

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"Build-Fix"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got != "Build-Fix" {
		t.Fatalf("rename callback got %q, want Build-Fix", got)
	}
	if out != "session renamed to Build-Fix" {
		t.Fatalf("output = %q", out)
	}
}

func TestRenameSession_PropagatesValidationError(t *testing.T) {
	tool := NewRenameSession(func(string) (string, error) {
		return "", errTestRename
	})

	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"bad name"}`)); err == nil {
		t.Fatal("expected error")
	}
}

// TestSessionNamePattern_AgreesWithNormalise guards against the advertised JSON
// Schema pattern drifting from the authoritative validator. Inputs are already
// trimmed and within the length cap, so the only rules left are charset and
// hyphen placement — exactly what the pattern must mirror.
func TestSessionNamePattern_AgreesWithNormalise(t *testing.T) {
	re := regexp.MustCompile(sessionNamePattern)
	names := []string{
		"build-fix", "Build-Fix", "BUILD-FIX", "api-2026-05", "a", "abc", "123",
		"bad name", "bad_name", "-bad", "bad-", "bad--name", "name.", "naïve",
	}
	for _, n := range names {
		patternOK := re.MatchString(n)
		_, err := session.NormaliseName(n)
		normaliseOK := err == nil
		if patternOK != normaliseOK {
			t.Errorf("disagreement for %q: pattern=%v normalise=%v", n, patternOK, normaliseOK)
		}
	}
}

type renameErr string

func (e renameErr) Error() string { return string(e) }

const errTestRename renameErr = "invalid name"
