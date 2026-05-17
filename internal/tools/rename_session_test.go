package tools

import (
	"context"
	"encoding/json"
	"testing"
)

func TestRenameSession_NormalisesName(t *testing.T) {
	var got string
	tool := NewRenameSession(func(name string) (string, error) {
		got = name
		return "BUILD-FIX", nil
	})

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"build-fix"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got != "build-fix" {
		t.Fatalf("rename callback got %q, want build-fix", got)
	}
	if out != "session renamed to BUILD-FIX" {
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
