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
// shows the remembered answer is still the decision's own: the sensitive file's
// caller is withheld exactly as when every node asked.
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
	total, ok := pack.related(goldID("cart/cart.go", "(*Cart).Total"))
	if !ok || !total.Withheld {
		t.Fatalf("control: the sensitive file's caller must still be withheld, got %+v (found %v)", total, ok)
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
