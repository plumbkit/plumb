package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/session"
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
// trimmed and within the length cap, so charset and hyphen placement are the
// only rules the pattern has to mirror — with one deliberate exception, below.
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

// TestSessionNamePattern_DoesNotEncodeTheReservedName documents the one place
// the schema pattern is deliberately WEAKER than NormaliseName: "next" is a
// well-formed name that the validator reserves for the mailbox's next-arrival
// address. The pattern is advertised as advisory (see sessionNamePattern), and
// a client-side regex is the wrong place to encode a server-side namespace
// rule — so the schema admits it and the server refuses it with a reason.
//
// Kept as its own test rather than an exception inside the agreement table: the
// table's contract is "these must agree", and burying a case that must NOT
// agree inside it is how that contract rots.
func TestSessionNamePattern_DoesNotEncodeTheReservedName(t *testing.T) {
	if !regexp.MustCompile(sessionNamePattern).MatchString("next") {
		t.Error("pattern rejects \"next\"; it is advisory and should admit it")
	}
	if _, err := session.NormaliseName("next"); err == nil {
		t.Error("NormaliseName accepted \"next\"; it is the reserved mailbox address")
	}
}

// TestRenameSession_NameTakenReachesTheAgentWithAReason pins the ErrNameTaken
// branch. The wrap must preserve errors.Is for callers, and must add WHY — an
// agent told only "taken" retries the same name.
func TestRenameSession_NameTakenReachesTheAgentWithAReason(t *testing.T) {
	tool := NewRenameSession(func(string) (string, error) {
		return "", fmt.Errorf("%w: %q", session.ErrNameTaken, "reviewer")
	})

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"reviewer"}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, session.ErrNameTaken) {
		t.Errorf("errors.Is(err, ErrNameTaken) = false; the wrap dropped the sentinel: %v", err)
	}
	if !strings.Contains(err.Error(), "reviewer") {
		t.Errorf("error does not name the conflicting name: %v", err)
	}
	if !strings.Contains(err.Error(), "pick another") {
		t.Errorf("error does not tell the agent what to do: %v", err)
	}
}

type renameErr string

func (e renameErr) Error() string { return string(e) }

const errTestRename renameErr = "invalid name"
