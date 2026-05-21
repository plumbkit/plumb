package tools

import (
	"context"
	"encoding/json"
	"testing"
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

type renameErr string

func (e renameErr) Error() string { return string(e) }

const errTestRename renameErr = "invalid name"
