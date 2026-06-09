package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/topology"
)

// emptyTopologyStore opens a throwaway topology store with nothing indexed, so
// tests can exercise the "live index, zero matches" path distinctly from a nil
// (disabled) store.
func emptyTopologyStore(t *testing.T) *topology.Store {
	t.Helper()
	st, err := topology.Open(t.TempDir(), config.Defaults().Topology, nil)
	if err != nil {
		t.Fatalf("opening empty topology store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestTopologySearch_EmptyResultNotDisabled is the F15 regression: store.Search
// returns a nil slice on no matches, which the formatter used to conflate with a
// nil (disabled) store — telling the agent topology was off and to edit config.
// A zero-match query against a live index must say "no results" instead.
func TestTopologySearch_EmptyResultNotDisabled(t *testing.T) {
	st := emptyTopologyStore(t)
	tool := NewTopologySearch(func() *topology.Store { return st })
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"zznosuchsymbol"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, "disabled") {
		t.Errorf("zero-match search must not report 'disabled'; got: %q", out)
	}
	if !strings.Contains(out, "no results") {
		t.Errorf("expected 'no results'; got: %q", out)
	}
}

// TestTopologyRoutes_EmptyResultNotDisabled is the same guarantee for routes:
// no matched patterns must read "no route patterns matched", not "disabled".
func TestTopologyRoutes_EmptyResultNotDisabled(t *testing.T) {
	st := emptyTopologyStore(t)
	tool := NewTopologyRoutes(func() *topology.Store { return st })
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, "disabled") {
		t.Errorf("no matched routes must not report 'disabled'; got: %q", out)
	}
	if !strings.Contains(out, "no route patterns matched") {
		t.Errorf("expected 'no route patterns matched'; got: %q", out)
	}
}

// TestTopologyTools_NilStoreStillReportsDisabled locks the other half of the
// fix: after moving the disabled message into Execute, a genuinely nil store
// must still report "disabled" for the tools that previously lacked coverage.
func TestTopologyTools_NilStoreStillReportsDisabled(t *testing.T) {
	nilFn := func() *topology.Store { return nil }
	cases := []struct {
		name string
		out  func() (string, error)
	}{
		{"impact", func() (string, error) {
			return NewTopologyImpact(nilFn).Execute(context.Background(), json.RawMessage(`{"name":"x"}`))
		}},
		{"affected", func() (string, error) {
			return NewTopologyAffected(nilFn).Execute(context.Background(), json.RawMessage(`{"files":["x.go"]}`))
		}},
		{"routes", func() (string, error) {
			return NewTopologyRoutes(nilFn).Execute(context.Background(), json.RawMessage(`{}`))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := c.out()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(out, "disabled") {
				t.Errorf("nil store must report 'disabled'; got: %q", out)
			}
		})
	}
}
