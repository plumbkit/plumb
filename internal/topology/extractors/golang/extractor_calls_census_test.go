package golang

import (
	"context"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

// callEdgesByName extracts src and returns its intra-file call edges as
// "caller->callee" by declaration name and receiver.
func callEdgesByName(t *testing.T, src string) map[string]bool {
	t.Helper()
	nodes, edges, err := New().Extract(context.Background(), "p.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, e := range edges {
		if e.Kind == topology.EdgeCalls {
			out[nodes[e.FromID].Qualified+"->"+nodes[e.ToID].Qualified] = true
		}
	}
	return out
}

// TestExtract_CallEdges_GenericInstantiation: `F[T](x)` calls F. The resolver files
// a same-file generic call as this pass's, so a miss here left the call with no
// edge at all, and the census called F's callers complete without it.
func TestExtract_CallEdges_GenericInstantiation(t *testing.T) {
	got := callEdgesByName(t, `package p

func Get[T any]() (v T) { return }

func Pair[K comparable, V any]() {}

func use() {
	_ = Get[int]()
	Pair[string, int]()
}

func paren() { _ = (Get[string])() }
`)
	for _, want := range []string{"use->Get", "use->Pair", "paren->Get"} {
		if !got[want] {
			t.Errorf("missing intra-file edge %s; got %v", want, got)
		}
	}
}

// TestExtract_CallEdges_AmbiguousNameIsNotGuessed: the pass matches by name alone,
// so when the file declares a name twice it cannot tell which one `a.Close()`
// means and emits no edge, rather than one to whichever was declared last. A name
// declared once in the same file is the control.
func TestExtract_CallEdges_AmbiguousNameIsNotGuessed(t *testing.T) {
	got := callEdgesByName(t, `package p

type A struct{}

func (A) Close() {}

type B struct{}

func (B) Close() {}

func helper() {}

func shut(a A) {
	a.Close()
	helper()
}
`)
	for e := range got {
		if strings.HasPrefix(e, "shut->") && strings.HasSuffix(e, "Close") {
			t.Errorf("an edge to one of two same-named declarations was guessed: %s", e)
		}
	}
	if !got["shut->helper"] {
		t.Errorf("control: shut->helper (declared once) is missing; got %v", got)
	}
}
