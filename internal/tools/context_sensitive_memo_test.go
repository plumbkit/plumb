package tools

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestContextForTask_AsksEachPathItsSensitivityOnce: a walk reaches many nodes of
// the same files, and the daemon's decision resolves the path's project on disk
// each time it is asked, so one call asks it once per path. The withheld control
// shows the remembered answer is still the decision's own: every node of the
// sensitive file is withheld, the repeated asks included, and none of its text
// reaches the pack.
func TestContextForTask_AsksEachPathItsSensitivityOnce(t *testing.T) {
	s := newShop(t)
	var mu sync.Mutex
	asked := map[string]int{}
	s.collector.WithSensitive(func(_ context.Context, p, _ string) bool {
		mu.Lock()
		asked[p]++
		mu.Unlock()
		return strings.HasSuffix(filepath.ToSlash(p), "/cart/cart.go")
	})
	pack := s.collect(t, map[string]any{"symbols": []string{"pricing/discount.go#Apply"}, "intent": contextIntentChange})

	if len(asked) == 0 {
		t.Fatal("control: the walk never asked about sensitivity, so the test proves nothing")
	}
	for p, n := range asked {
		if n != 1 {
			t.Errorf("%s was asked %d times in one call, want once", p, n)
		}
	}
	// Every node from the sensitive file is withheld, not only the first one asked
	// about: a memo that remembered the wrong answer would leak the later ones.
	var fromCart int
	for _, r := range pack.Related {
		if r.Node.Path != "cart/cart.go" {
			continue
		}
		fromCart++
		if !r.Withheld {
			t.Errorf("%s from the sensitive cart/cart.go is not withheld: %+v", nodeSelector(r.Node), r)
		}
	}
	if fromCart < 2 {
		t.Fatalf("control: the walk reached %d node(s) of cart/cart.go, want at least 2, so a repeated answer is tested", fromCart)
	}
	out := renderContextPack(pack)
	for _, body := range []string{"kept := c.items[:0]", "sub += it.Cents * it.Qty", "c.items[i].Qty += it.Qty"} {
		if strings.Contains(out, body) {
			t.Errorf("content of the sensitive cart/cart.go reached the pack: %q", body)
		}
	}

	// A second call asks again: the memory lasts one call, never across calls.
	before := len(asked)
	_ = s.collect(t, map[string]any{"symbols": []string{"pricing/discount.go#Apply"}, "intent": contextIntentChange})
	for p, n := range asked {
		if n != 2 {
			t.Errorf("%s was asked %d times over two calls, want twice (once per call)", p, n)
		}
	}
	if len(asked) != before {
		t.Errorf("the second call reached %d paths, the first %d: the walk is not deterministic", len(asked), before)
	}
}
