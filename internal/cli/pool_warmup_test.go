package cli

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestWarmupFor covers the three states warmupFor distinguishes: no entry, a
// warming entry (nil proxy), and a ready entry.
func TestWarmupFor(t *testing.T) {
	p := &workspacePool{entries: make(map[poolKey]*poolEntry), baseCtx: context.Background()}

	if warming, _ := p.warmupFor("/x", "go"); warming {
		t.Fatal("expected not-warming for an absent entry")
	}

	e := &poolEntry{root: "/x", language: "go", proxy: &clientProxy{}, startedAt: time.Now().Add(-3 * time.Second)}
	p.entries[poolKey{root: "/x", language: "go"}] = e
	warming, elapsed := p.warmupFor("/x", "go")
	if !warming {
		t.Fatal("expected warming for an entry whose proxy is still nil")
	}
	if elapsed < 2*time.Second {
		t.Fatalf("expected elapsed ~3s, got %s", elapsed)
	}

	e.proxy.set(&stubClient{})
	if warming, _ := p.warmupFor("/x", "go"); warming {
		t.Fatal("expected not-warming once the proxy is ready")
	}
}

// TestWarmingErr checks the warm-up error folds in elapsed time, the routed
// root, and a pointer to the tools that answer immediately.
func TestWarmingErr(t *testing.T) {
	zero := warmingErr(0, "").Error()
	if !strings.Contains(zero, "not yet ready") || !strings.Contains(zero, "topology_search") {
		t.Fatalf("zero-elapsed message unexpected: %q", zero)
	}

	got := warmingErr(3400*time.Millisecond, "/proj").Error()
	if !strings.Contains(got, "still warming for /proj") {
		t.Fatalf("expected routed root in message: %q", got)
	}
	if !strings.Contains(got, "3s elapsed") {
		t.Fatalf("expected rounded elapsed in message: %q", got)
	}
}

func TestRoundWarmElapsed(t *testing.T) {
	if got := roundWarmElapsed(450 * time.Millisecond); got != 500*time.Millisecond {
		t.Fatalf("sub-second rounding: got %s", got)
	}
	if got := roundWarmElapsed(3400 * time.Millisecond); got != 3*time.Second {
		t.Fatalf("multi-second rounding: got %s", got)
	}
}
