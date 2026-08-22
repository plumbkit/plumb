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
	err := lspTimeoutErr("workspace_symbols", time.Second, sentinel)
	if !errors.Is(err, sentinel) {
		t.Errorf("expected the underlying error to be wrapped, got: %v", err)
	}
	if !strings.Contains(err.Error(), "workspace_symbols") {
		t.Errorf("expected the tool name in the message, got: %v", err)
	}
}

// TestWithFallbackLSPDeadline_LeavesHeadroom is the structural half of the
// PLAN-390 guard: the language-server attempt of a fallback-capable tool must
// end STRICTLY BEFORE the time available to the tool, so the tree-sitter parse
// still has a live context to run on. Re-point read_symbol at withLSPDeadline
// and the third case here goes red, because that helper hands an
// already-bounded context straight through.
func TestWithFallbackLSPDeadline_LeavesHeadroom(t *testing.T) {
	t.Run("no caller deadline", func(t *testing.T) {
		const timeout = time.Second
		start := time.Now()
		ctx, cancel, budget := withFallbackLSPDeadline(context.Background(), timeout)
		defer cancel()
		dl, ok := ctx.Deadline()
		if !ok {
			t.Fatal("expected the attempt to be bounded")
		}
		if !dl.Before(start.Add(timeout)) {
			t.Errorf("attempt deadline %v is not before the budget end %v — no headroom for the fallback",
				dl.Sub(start), timeout)
		}
		if budget <= 0 || budget >= timeout {
			t.Errorf("reported budget %v must be positive and shorter than %v", budget, timeout)
		}
	})

	t.Run("caller deadline shorter than the timeout", func(t *testing.T) {
		parent, pcancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer pcancel()
		pdl, _ := parent.Deadline()
		ctx, cancel, budget := withFallbackLSPDeadline(parent, time.Hour)
		defer cancel()
		dl, ok := ctx.Deadline()
		if !ok {
			t.Fatal("expected the attempt to be bounded by the caller's deadline")
		}
		if !dl.Before(pdl) {
			t.Errorf("attempt deadline %v is not before the caller's %v — the caller's whole "+
				"budget goes to the server and the fallback can never run", dl, pdl)
		}
		if budget >= 200*time.Millisecond {
			t.Errorf("budget %v consumed the caller's entire remaining time", budget)
		}
	})

	t.Run("caller deadline with the cap disabled", func(t *testing.T) {
		parent, pcancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer pcancel()
		pdl, _ := parent.Deadline()
		ctx, cancel, budget := withFallbackLSPDeadline(parent, 0)
		defer cancel()
		dl, ok := ctx.Deadline()
		if !ok {
			t.Fatal("expected the caller's deadline to still bound the attempt")
		}
		if !dl.Before(pdl) {
			t.Errorf("with [lsp_query] disabled the attempt still took the caller's whole "+
				"budget (%v vs %v): a fallback-capable tool must always reserve headroom", dl, pdl)
		}
		if budget <= 0 {
			t.Errorf("expected a positive budget derived from the caller's deadline, got %v", budget)
		}
	})

	t.Run("no bound at all", func(t *testing.T) {
		ctx, cancel, budget := withFallbackLSPDeadline(context.Background(), 0)
		defer cancel()
		if _, ok := ctx.Deadline(); ok {
			t.Error("a disabled timeout with no caller deadline must not impose one")
		}
		if budget != 0 {
			t.Errorf("expected a zero budget when uncapped, got %v", budget)
		}
	})
}
