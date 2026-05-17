package tools

import (
	"sync"
	"testing"
	"time"

	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// stubDiag is a thread-safe postWriteDiagSource for testing awaitDiagnosticsRefresh.
type stubDiag struct {
	mu   sync.Mutex
	diag []protocol.Diagnostic
}

func (s *stubDiag) Diagnostics(_ string) []protocol.Diagnostic {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.diag
}

func (s *stubDiag) set(d []protocol.Diagnostic) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.diag = d
}

func errDiag(msg string) []protocol.Diagnostic {
	return []protocol.Diagnostic{{Severity: protocol.SevError, Message: msg}}
}

func TestAwaitDiagnosticsRefresh_NilSource(t *testing.T) {
	got := awaitDiagnosticsRefresh(nil, "file:///foo.go", nil, 50*time.Millisecond)
	if got != nil {
		t.Errorf("nil source: want nil, got %v", got)
	}
}

func TestAwaitDiagnosticsRefresh_Disabled(t *testing.T) {
	src := &stubDiag{}
	baseline := errDiag("old error")
	src.set(baseline)

	start := time.Now()
	got := awaitDiagnosticsRefresh(src, "file:///foo.go", baseline, -1)
	elapsed := time.Since(start)

	if elapsed > 20*time.Millisecond {
		t.Errorf("disabled window: returned after %v, want near-instant", elapsed)
	}
	if len(got) != 1 || got[0].Message != "old error" {
		t.Errorf("disabled window: want baseline returned unchanged, got %v", got)
	}
}

func TestAwaitDiagnosticsRefresh_TimesOut(t *testing.T) {
	src := &stubDiag{}
	baseline := errDiag("unchanged")
	src.set(baseline)

	window := 60 * time.Millisecond
	start := time.Now()
	got := awaitDiagnosticsRefresh(src, "file:///foo.go", baseline, window)
	elapsed := time.Since(start)

	if elapsed < window {
		t.Errorf("should have waited at least %v, returned after %v", window, elapsed)
	}
	if len(got) != 1 || got[0].Message != "unchanged" {
		t.Errorf("timeout: want baseline, got %v", got)
	}
}

func TestAwaitDiagnosticsRefresh_EarlyReturn(t *testing.T) {
	src := &stubDiag{}
	baseline := errDiag("before")
	src.set(baseline)

	// Change the diagnostics after a short delay.
	go func() {
		time.Sleep(30 * time.Millisecond)
		src.set(errDiag("after"))
	}()

	window := 500 * time.Millisecond
	start := time.Now()
	got := awaitDiagnosticsRefresh(src, "file:///foo.go", baseline, window)
	elapsed := time.Since(start)

	if elapsed >= window {
		t.Errorf("should have returned early (diag changed), but waited full window %v", elapsed)
	}
	if len(got) != 1 || got[0].Message != "after" {
		t.Errorf("early return: want updated diag, got %v", got)
	}
}

func TestAwaitDiagnosticsRefresh_ZeroWindowUsesDefault(t *testing.T) {
	src := &stubDiag{}
	baseline := errDiag("no change")
	src.set(baseline)

	// window=0 should use defaultPostWriteDiagWindow (300ms). We use a change
	// fired after 50ms to confirm we didn't return instantly (which would mean
	// the window was treated as 0 duration rather than the 300ms default).
	changed := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		src.set(errDiag("changed"))
		close(changed)
	}()

	got := awaitDiagnosticsRefresh(src, "file:///foo.go", baseline, 0)

	select {
	case <-changed:
	default:
		t.Fatal("goroutine should have fired before the 300ms default window expired")
	}
	if len(got) != 1 || got[0].Message != "changed" {
		t.Errorf("zero window: want updated diag via default window, got %v", got)
	}
}
