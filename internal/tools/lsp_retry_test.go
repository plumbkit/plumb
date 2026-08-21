package tools

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestIsServerNotReadyErr_MatchesObservedPhrasing pins the two exact error
// families the 2026-08 error autopsy identified for sourcekit-lsp (jsonrpc
// -32001 "No language service for" and "Failed to find snapshot for"), plus a
// negative case so an unrelated failure is never silently retried.
func TestIsServerNotReadyErr_MatchesObservedPhrasing(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"no language service", errors.New(`sourcekit-lsp documentSymbol: jsonrpc error -32001: No language service for 'file:///p/x.swift' found`), true},
		{"failed to find snapshot", errors.New(`sourcekit-lsp rename: jsonrpc error -32001: Failed to find snapshot for 'file:///p/x.swift'`), true},
		{"case insensitive", errors.New(`NO LANGUAGE SERVICE FOR 'x'`), true},
		{"unrelated timeout", context.DeadlineExceeded, false},
		{"position miss", errors.New("gopls definition: no identifier found"), false},
		{"stale index", errors.New("references: index is stale after recent edits"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isServerNotReadyErr(c.err); got != c.want {
				t.Errorf("isServerNotReadyErr(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestRetryOnServerNotReady_SucceedsOnFirstTry never sleeps or re-invokes call
// when the first attempt succeeds.
func TestRetryOnServerNotReady_SucceedsOnFirstTry(t *testing.T) {
	calls := 0
	got, err := retryOnServerNotReady(context.Background(), func() (string, error) {
		calls++
		return "ok", nil
	})
	if err != nil || got != "ok" {
		t.Fatalf("got (%q, %v), want (ok, nil)", got, err)
	}
	if calls != 1 {
		t.Errorf("expected exactly 1 call, got %d", calls)
	}
}

// TestRetryOnServerNotReady_RetriesOnceThenSucceeds is the PLAN-363 item-5
// repro: a "No language service for" rejection — the sourcekit-lsp race
// between didOpen and the server's own per-document readiness — is retried
// once and, when the second attempt succeeds, never surfaces to the caller.
func TestRetryOnServerNotReady_RetriesOnceThenSucceeds(t *testing.T) {
	calls := 0
	got, err := retryOnServerNotReady(context.Background(), func() (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("No language service for 'file:///p/x.swift' found")
		}
		return "ok", nil
	})
	if err != nil || got != "ok" {
		t.Fatalf("got (%q, %v), want (ok, nil) after one retry", got, err)
	}
	if calls != 2 {
		t.Errorf("expected exactly 2 calls (one retry), got %d", calls)
	}
}

// TestRetryOnServerNotReady_UnrelatedErrorNeverRetries asserts the retry is
// narrowly scoped: a position-miss or any other failure is returned after the
// FIRST attempt, never masked by a retry that could hide a different bug.
func TestRetryOnServerNotReady_UnrelatedErrorNeverRetries(t *testing.T) {
	calls := 0
	wantErr := errors.New("no identifier found")
	_, err := retryOnServerNotReady(context.Background(), func() (string, error) {
		calls++
		return "", wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected the original error unwrapped, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected exactly 1 call for an unrelated error, got %d", calls)
	}
}

// TestRetryOnServerNotReady_CancelledContextSkipsRetry asserts the retry wait
// is governed by ctx: a context that is already done returns the FIRST
// attempt's result without a second call, so this can never make a caller
// wait past its own deadline.
func TestRetryOnServerNotReady_CancelledContextSkipsRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	_, err := retryOnServerNotReady(ctx, func() (string, error) {
		calls++
		return "", errors.New("No language service for 'file:///p/x.swift' found")
	})
	if err == nil {
		t.Fatal("expected the original not-ready error to surface")
	}
	if calls != 1 {
		t.Errorf("expected exactly 1 call when ctx is already done, got %d", calls)
	}
}

// TestRetryOnServerNotReady_DelayIsShort is a coarse guard that the retry
// delay stays a narrow indexing-race wait, not a poll loop: the whole
// retry-then-succeed round trip must complete well under one second.
func TestRetryOnServerNotReady_DelayIsShort(t *testing.T) {
	start := time.Now()
	calls := 0
	_, _ = retryOnServerNotReady(context.Background(), func() (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("No language service for 'x' found")
		}
		return "ok", nil
	})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("retry took %s, want well under 1s", elapsed)
	}
}
