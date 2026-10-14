package tools

import (
	"context"
	"sync"
	"testing"
)

// TestCallMemo_RemembersPerKeyAndNilRemembersNothing: a memo runs a key's lookup
// once; a nil memo (a context no collect made) runs it on every ask.
func TestCallMemo_RemembersPerKeyAndNilRemembersNothing(t *testing.T) {
	m := CallMemoFrom(WithCallMemo(context.Background()))
	runs := map[string]int{}
	look := func(key string) func() string {
		return func() string { runs[key]++; return "root-" + key }
	}
	for range 3 {
		if got := m.Once("a", look("a")); got != "root-a" {
			t.Fatalf("Once(a) = %q", got)
		}
	}
	m.Once("b", look("b"))
	if runs["a"] != 1 || runs["b"] != 1 {
		t.Errorf("lookups ran %v, want each key once", runs)
	}

	var none *CallMemo
	if CallMemoFrom(context.Background()) != nil {
		t.Fatal("a context no collect made carries a memo")
	}
	none.Once("c", look("c"))
	none.Once("c", look("c"))
	if runs["c"] != 2 {
		t.Errorf("a nil memo ran its lookup %d times over two asks, want twice", runs["c"])
	}
}

// TestContextForTask_SensitiveAsksShareOneMemoPerCall: every ask one call makes
// of the sensitive-path decision carries the same memo, and the next call a fresh
// one, so a remembered answer never outlives the call that asked.
func TestContextForTask_SensitiveAsksShareOneMemoPerCall(t *testing.T) {
	s := newShop(t)
	var mu sync.Mutex
	var seen []*CallMemo
	s.collector.WithSensitive(func(ctx context.Context, _, _ string) bool {
		mu.Lock()
		seen = append(seen, CallMemoFrom(ctx))
		mu.Unlock()
		return false
	})
	args := map[string]any{"symbols": []string{"pricing/discount.go#Apply"}, "intent": contextIntentChange}

	s.collect(t, args)
	first := len(seen)
	if first == 0 {
		t.Fatal("control: the call never asked about sensitivity, so the test proves nothing")
	}
	s.collect(t, args)
	for i, m := range seen {
		if m == nil {
			t.Fatalf("ask %d carried no memo", i)
		}
		if want := seen[0]; i < first && m != want {
			t.Errorf("ask %d of the first call carried a different memo", i)
		}
	}
	if len(seen) == first || seen[first] == seen[0] {
		t.Error("the second call reused the first call's memo: its answers would outlive the call")
	}
}
