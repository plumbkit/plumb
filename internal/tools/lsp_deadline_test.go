package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWithLSPDeadline_AddsDeadlineWhenNone(t *testing.T) {
	ctx, cancel := withLSPDeadline(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("expected a deadline to be set when the parent has none")
	}
}

func TestWithLSPDeadline_DisabledWhenZero(t *testing.T) {
	ctx, cancel := withLSPDeadline(context.Background(), 0)
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("expected no deadline when timeout is zero (disabled)")
	}
}

func TestWithLSPDeadline_PreservesExistingDeadline(t *testing.T) {
	parent, cancelParent := context.WithTimeout(context.Background(), time.Hour)
	defer cancelParent()
	want, _ := parent.Deadline()

	ctx, cancel := withLSPDeadline(parent, 50*time.Millisecond)
	defer cancel()
	got, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected the existing deadline to be preserved")
	}
	if !got.Equal(want) {
		t.Errorf("deadline was changed: got %v, want %v", got, want)
	}
}

func TestLSPTimeoutErr_RewritesDeadlineExceeded(t *testing.T) {
	err := lspTimeoutErr("workspace_symbols", 30*time.Second, context.DeadlineExceeded)
	msg := err.Error()
	if !strings.Contains(msg, "workspace_symbols") {
		t.Errorf("expected the tool name in the message, got: %q", msg)
	}
	if !strings.Contains(msg, "did not respond within 30s") {
		t.Errorf("expected the friendly timeout message, got: %q", msg)
	}
}

func TestLSPTimeoutErr_WrapsOtherErrors(t *testing.T) {
	sentinel := errors.New("boom")
	err := lspTimeoutErr("find_symbol", time.Second, sentinel)
	if !errors.Is(err, sentinel) {
		t.Errorf("expected the underlying error to be wrapped, got: %v", err)
	}
	if !strings.Contains(err.Error(), "find_symbol") {
		t.Errorf("expected the tool name in the message, got: %v", err)
	}
}
