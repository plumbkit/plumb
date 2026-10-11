package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/topology"
)

// context_followup_test.go — a follow-up call the pack prints is a promise: running it
// must return the body the pack left out. The tests below run what the pack printed
// through the real read_symbol tool with no language server, which is the route that
// refused the Go receiver spelling "(*Recv).Method" the index uses for a method.

// followUpSource declares each function with a body too large for the small budgets
// below, so the pack prints the call to read it instead of the body.
var followUpSource = strings.NewReplacer("PAD", strings.Repeat("\t// padding that keeps this body larger than the smallest budgets\n", 40)).Replace(`package p

type S struct{}

func f() int {
PAD	return 1
}

func (s *S) M() int {
PAD	return f()
}

func (s S) V() int {
PAD	return 2
}

type G[T any] struct{ v T }

func (g *G[T]) Get() T {
PAD	return g.v
}
`)

// followUpRead is read_symbol as an agent without a language server has it.
func followUpRead(root string, store *topology.Store) *ReadSymbol {
	return NewReadSymbol(&unavailableLSP{}, nil, 0, 2*time.Second, NewReadTracker()).
		WithTopologyFallback(func() *topology.Store { return store }).
		WithBoundary(testBoundaryGuard(root)).
		WithWorkspace(func(context.Context) string { return root })
}

// followUpCalls decodes the arguments of every read_symbol call the pack printed.
func followUpCalls(t *testing.T, out string) []json.RawMessage {
	t.Helper()
	var calls []json.RawMessage
	for _, line := range strings.Split(out, "\n") {
		_, rest, ok := strings.Cut(line, "read_symbol {")
		if !ok {
			continue
		}
		var raw json.RawMessage
		dec := json.NewDecoder(strings.NewReader("{" + rest))
		if err := dec.Decode(&raw); err != nil {
			t.Fatalf("the printed call is not JSON: %q: %v", line, err)
		}
		calls = append(calls, raw)
	}
	return calls
}

// firstFollowUp collects seed at the smallest budget at which the pack prints a
// follow-up call (a budget that cannot hold the body but can hold the line that names
// how to get it), and returns that response and the calls in it.
func (s shopTool) firstFollowUp(t *testing.T, seed string) (string, []json.RawMessage) {
	t.Helper()
	var out string
	for _, budget := range []int{contextMinMaxBytes, 1700, 1900, 2100, 2400, 2800, 3400, 4200} {
		var err error
		if out, err = s.run(t, map[string]any{"symbols": []string{seed}, "max_bytes": budget}); err != nil {
			t.Fatal(err)
		}
		if calls := followUpCalls(t, out); len(calls) > 0 {
			return out, calls
		}
	}
	return out, nil
}

func callName(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var a struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	return a.Name
}

// runFollowUp runs a printed call through read_symbol and returns what it said.
func runFollowUp(rs *ReadSymbol, raw json.RawMessage) (string, error) {
	return rs.Execute(context.Background(), raw)
}

func TestContextForTask_FollowUpCallsRunThroughReadSymbolWithoutALanguageServer(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "s.go"), []byte(followUpSource), 0o600); err != nil {
		t.Fatal(err)
	}
	store := openContextStore(t, root, 1)
	s := newShopTool(t, store, root)
	rs := followUpRead(canonicalRoot(root), store)

	for _, tc := range []struct {
		name, seed, wantName, wantBody string
	}{
		{"pointer receiver", "s.go#S.M", "S.M", "return f()"},
		{"value receiver", "s.go#S.V", "S.V", "return 2"},
		{"generic receiver", "s.go#Get", "G.Get", "return g.v"},
		{"exact function (positive control)", "s.go#f", "f", "return 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, calls := s.firstFollowUp(t, tc.seed)
			if len(calls) == 0 {
				t.Fatalf("control: no budget made the pack print a follow-up call:\n%s", out)
			}
			if got := callName(t, calls[0]); got != tc.wantName {
				t.Errorf("the follow-up call names %q, want %q:\n%s", got, tc.wantName, out)
			}
			body, err := runFollowUp(rs, calls[0])
			if err != nil {
				t.Fatalf("read_symbol refused the call the pack printed: %v\n%s", err, calls[0])
			}
			if !strings.Contains(body, tc.wantBody) {
				t.Errorf("read_symbol returned no body for the printed call %s:\n%s", calls[0], body)
			}
		})
	}
}

// The fallback still refuses the spelling the index uses for a method. This is the
// premise of the fix, and what makes the test above fail without it: if read_symbol
// learns to accept "(*Recv).Method" (a separate change), delete this control with it.
func TestContextForTask_FollowUpControl_TheFallbackRefusesTheReceiverSpelling(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "s.go"), []byte(followUpSource), 0o600); err != nil {
		t.Fatal(err)
	}
	store := openContextStore(t, root, 1)
	rs := followUpRead(canonicalRoot(root), store)
	for name, wantBody := range map[string]bool{"(*S).M": false, "S.M": true, "M": true} {
		raw, _ := json.Marshal(map[string]string{"path": filepath.Join(canonicalRoot(root), "s.go"), "name": name})
		out, err := runFollowUp(rs, raw)
		if got := err == nil && strings.Contains(out, "return f()"); got != wantBody {
			t.Errorf("read_symbol name=%q returned the body = %v, want %v (err=%v)", name, got, wantBody, err)
		}
	}
}

// The related-declaration and next: call sites render through the same spelling, so
// every call printed for a fixture pack runs, at every budget that prints one.
func TestContextForTask_EveryPrintedFollowUpRunsOnTheShopFixture(t *testing.T) {
	s := newShop(t)
	rs := followUpRead(canonicalRoot(s.root), s.store)
	related := 0
	for seed, own := range map[string]string{"cart/cart.go#(*Cart).Total": "Cart.Total", "pricing/discount.go#Apply": "Apply"} {
		for _, budget := range []int{contextMinMaxBytes, 2000, 2600, 3200, 4500, 7000} {
			out, err := s.run(t, map[string]any{"symbols": []string{seed}, "intent": contextIntentChange, "max_bytes": budget})
			if err != nil {
				t.Fatal(err)
			}
			for _, raw := range followUpCalls(t, out) {
				body, err := runFollowUp(rs, raw)
				if err != nil || strings.Contains(body, "No symbol named") {
					t.Errorf("seed %s at %d bytes: read_symbol cannot run %s: %v %s", seed, budget, raw, err, body)
				}
				if callName(t, raw) != own {
					related++
				}
			}
		}
	}
	if related == 0 {
		t.Error("control: no related declaration's follow-up call was printed, so that call site was not exercised")
	}
}

func TestNextCall_UsesASpellingReadSymbolAccepts(t *testing.T) {
	got := nextCall(contextSeed{Kind: seedSymbol, Abs: "/r/s.go", Selector: "(*S).M", Name: "M"})
	if want := `read_symbol {"path":"/r/s.go","name":"S.M"}`; got != want {
		t.Errorf("nextCall = %s, want %s", got, want)
	}
}

func TestReadSymbolName(t *testing.T) {
	for _, tc := range []struct{ selector, name, want string }{
		{"(*Cart).Total", "Total", "Cart.Total"},
		{"(Cart).Total", "Total", "Cart.Total"},
		{"(*Box[T]).Get", "Get", "Box.Get"},
		{"(*Pair[K, V]).Get", "Get", "Pair.Get"},
		{"Cart.Add", "Add", "Cart.Add"},   // a dotted selector both routes understand stays
		{"Outer.Inner.run", "run", "run"}, // nested: neither route resolves it, the bare name does
		{"Foo::run", "run", "run"},
		{"total", "total", "total"},
		{"(*).M", "M", "M"}, // not a receiver spelling
		{"", "x", "x"},
		{"a.b", "", "a.b"}, // no name to fall back on
	} {
		if got := readSymbolName(tc.selector, tc.name); got != tc.want {
			t.Errorf("readSymbolName(%q, %q) = %q, want %q", tc.selector, tc.name, got, tc.want)
		}
	}
}
