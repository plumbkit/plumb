package tools

import (
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

// TestCouldCall pins which open calls of a name could be calls of a declaration
// in internal/lib. Each false row is a call a "complete" claim may ignore; each
// true row is one it must not.
func TestCouldCall(t *testing.T) {
	const dir = "internal/lib"
	call := func(p, kind string, dot bool) topology.OpenCall {
		return topology.OpenCall{Path: p, Kind: kind, DotImport: dot}
	}
	for _, c := range []struct {
		name             string
		o                topology.OpenCall
		method, exported bool
		want             bool
	}{
		{"bare call of a function, own package", call("internal/lib/a.go", "bare", false), false, true, true},
		{"bare call of a function, another package", call("internal/app/a.go", "bare", false), false, true, false},
		{"bare call through a dot import", call("internal/app/a.go", "bare", true), false, true, true},
		{"dot import cannot reach an unexported name", call("internal/app/a.go", "bare", true), false, false, false},
		{"bare call is never a method", call("internal/lib/a.go", "bare", false), true, true, false},
		{"block-scoped local may still be the function", call("internal/lib/a.go", "local", false), false, false, true},
		{"receiver call of a method", call("internal/app/a.go", "receiver", false), true, true, true},
		{"receiver call is never a function", call("internal/app/a.go", "receiver", false), false, true, false},
		{"unexported method from another package", call("internal/app/a.go", "receiver", false), true, false, false},
		{"package value's method (unmatched)", call("internal/app/a.go", "unmatched", false), true, true, true},
		{"top-level call could be either", call("internal/app/a.go", "top-level", false), false, true, true},
		{"same-file ambiguity could be either", call("internal/lib/a.go", "same-file", false), true, false, true},
		{"unread call could be either", call("internal/app/a.go", "unresolved", false), true, true, true},
	} {
		if got := couldCall(c.o, c.method, c.exported, dir); got != c.want {
			t.Errorf("%s: couldCall = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestContextForTask_CensusCoversAFileSeedsDeclarations: agents mostly pass files,
// so a file seed's top declarations get the census too, with the shared caveat
// said once. Control: the open calls of Total are named, not claimed complete.
func TestContextForTask_CensusCoversAFileSeedsDeclarations(t *testing.T) {
	s := newShop(t)
	out, err := s.run(t, map[string]any{"files": []string{"cart/cart.go"}, "max_bytes": 32000})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "callers of (*Cart).Total: 0 in the index") ||
		!strings.Contains(out, "unresolved call(s) of Total may also be it: api/checkout.go:13 (receiver)") {
		t.Errorf("a file seed's declaration has no census line:\n%s", out)
	}
	if n := strings.Count(out, labelCensus); n != 1 {
		t.Errorf("the census caveat appears %d times, want once:\n%s", n, out)
	}
	if strings.Contains(out, "callers of (*Cart).Total: complete") {
		t.Errorf("open calls of Total were claimed complete:\n%s", out)
	}
}
