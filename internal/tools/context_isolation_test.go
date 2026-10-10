package tools

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

// context_isolation_test.go — Invariant 3 (PLAN-462 A4): topology and memory come
// through accessors keyed by the calling agent's canonical root, never the
// connection's. Two agents share one connection and are pinned to two roots; one
// root holds private material the other must never see.

const (
	memoryMarkerA = "ALPHA-ONLY-MEMORY-MARKER-91c2"
	memoryMarkerB = "BRAVO-ONLY-MEMORY-MARKER-4d7e"
)

// twoRoots is one connection and two roots. The connection's own pin is root A
// (what a connection-keyed collector would answer every agent from); agent "a" is
// pinned to A and agent "b" to B. Root A holds private/notes.md (the fixture's
// private marker), a memory carrying memoryMarkerA and a function that exists only
// there; root B holds a memory with memoryMarkerB, a function of its own and no
// private tree.
type twoRoots struct {
	rootA, rootB   string
	storeA, storeB *topology.Store
	collector      *ContextCollector
	tool           *ContextForTask
	storeFor       TopologyForRootFn
}

func newTwoRoots(t *testing.T) twoRoots {
	t.Helper()
	w := twoRoots{rootA: copyShopFixture(t), rootB: copyShopFixture(t)}
	writeMemory(t, w.rootA, "alpha-private", "description: alpha rules\npaths: cart/*.go\n", memoryMarkerA+"\n")
	writeFiles(t, w.rootA, map[string]string{"pricing/only_a.go": "package pricing\n\n// OnlyInA exists in root A alone.\nfunc OnlyInA() int { return 1 }\n"})
	if err := os.RemoveAll(filepath.Join(w.rootB, "private")); err != nil {
		t.Fatal(err)
	}
	writeMemory(t, w.rootB, "bravo-notes", "description: bravo rules\npaths: cart/*.go\n", memoryMarkerB+"\n")
	writeFiles(t, w.rootB, map[string]string{"pricing/only_b.go": "package pricing\n\n// OnlyInB exists in root B alone.\nfunc OnlyInB() int { return 2 }\n"})
	w.storeA = openContextStore(t, w.rootA, shopFixtureFiles+1)
	w.storeB = openContextStore(t, w.rootB, shopFixtureFiles)

	ws := func(ctx context.Context) string {
		switch agent, _ := ctx.Value(ctxKeyAgent{}).(string); agent {
		case "a":
			return w.rootA
		case "b":
			return w.rootB
		}
		return w.rootA // the connection's own pin
	}
	w.storeFor = func(root string) *topology.Store {
		switch canonicalRoot(root) {
		case canonicalRoot(w.rootA):
			return w.storeA
		case canonicalRoot(w.rootB):
			return w.storeB
		}
		return nil
	}
	guard := func(ctx context.Context, p string) error { return testBoundaryGuard(ws(ctx))(ctx, p) }
	w.collector = NewContextCollector(w.storeFor).WithWorkspace(ws).WithBoundary(guard)
	w.tool = NewContextForTask(w.collector)
	return w
}

func (w twoRoots) run(t *testing.T, ctx context.Context, args map[string]any) (string, error) {
	t.Helper()
	return w.tool.Execute(ctx, mustJSON(args))
}

// The headline test. The same request is made by both agents. Agent A, at its own
// root, sees its private memory and private file (the positive control: the
// material is reachable by this very call). Agent B never sees any of it, in the
// pack, in a path, or in a refusal. B sees its own memory, so its pack is not empty
// for another reason.
func TestContextForTask_TwoAgentsTwoRoots_PrivateMaterialNeverCrossesRoots(t *testing.T) {
	w := newTwoRoots(t)
	args := map[string]any{"symbols": []string{"cart/cart.go#Cart.Total"}, "task": "Summarise the pricing notes and rules."}

	outA, err := w.run(t, asAgent("a"), args)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{memoryMarkerA, privateMarker, "path=private/notes.md]", "alpha-private"} {
		if !strings.Contains(outA, want) {
			t.Fatalf("control: agent A at its own root did not see %q, so B's absence proves nothing:\n%s", want, outA)
		}
	}
	if strings.Contains(outA, memoryMarkerB) {
		t.Errorf("agent A saw root B's memory:\n%s", outA)
	}

	outB, err := w.run(t, asAgent("b"), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(outB, memoryMarkerB) || !strings.Contains(outB, "body lines ") {
		t.Fatalf("control: agent B's own pack lacks its own memory or body:\n%s", outB)
	}
	for _, forbidden := range []string{memoryMarkerA, privateMarker, "alpha-private", "private/notes.md", canonicalRoot(w.rootA), w.rootA} {
		if strings.Contains(outB, forbidden) {
			t.Errorf("agent B's pack contains %q, which belongs to root A:\n%s", forbidden, outB)
		}
	}

	// Naming root A's private file does not reach it either: relative, it is not in
	// B's root; absolute, the boundary refuses it, and the refusal does not quote it.
	for _, file := range []string{"private/notes.md", filepath.Join(w.rootA, "private", "notes.md")} {
		out, err := w.run(t, asAgent("b"), map[string]any{"files": []string{file}})
		if strings.Contains(out, privateMarker) || (err != nil && strings.Contains(err.Error(), privateMarker)) {
			t.Errorf("seeding %s as agent B reached the private marker (out %q, err %v)", file, out, err)
		}
	}
}

// Symbols resolve in the index of the agent's own root and in no other. A symbol
// that exists only in the other root is unresolved, with no candidate from that
// index and no mention of its file. The connection's own call (no agent) is root A,
// which is the view the old connection-keyed design gave every agent.
func TestContextForTask_TwoAgentsTwoRoots_SymbolsResolveInTheAgentsOwnIndex(t *testing.T) {
	w := newTwoRoots(t)
	resolves := func(ctx context.Context, symbol string) (resolved bool, out string) {
		t.Helper()
		pack := func() contextPack {
			raw := mustJSON(map[string]any{"symbols": []string{symbol}})
			req, err := parseContextRequest(raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := req.normalise(); err != nil {
				t.Fatal(err)
			}
			p, err := w.collector.Collect(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			return p
		}()
		out, err := w.run(t, ctx, map[string]any{"symbols": []string{symbol}})
		if err != nil {
			t.Fatal(err)
		}
		return len(pack.Seeds) == 1, out
	}
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		symbol string
		want   bool
	}{
		{"A sees its own function", asAgent("a"), "OnlyInA", true},
		{"B does not see A's", asAgent("b"), "OnlyInA", false},
		{"B sees its own function", asAgent("b"), "OnlyInB", true},
		{"A does not see B's", asAgent("a"), "OnlyInB", false},
		{"the connection is root A", context.Background(), "OnlyInA", true},
		{"the connection does not see B's", context.Background(), "OnlyInB", false},
	} {
		got, out := resolves(tc.ctx, tc.symbol)
		if got != tc.want {
			t.Errorf("%s: resolved = %v, want %v\n%s", tc.name, got, tc.want, out)
		}
		if !tc.want && (strings.Contains(out, "only_a.go") || strings.Contains(out, "only_b.go")) {
			t.Errorf("%s: the other root's file is named:\n%s", tc.name, out)
		}
	}
}

// The hint side is keyed by the request's workspace alone: the connection's pin
// (root A here) is never consulted.
func TestContextHint_TwoRootsAreAnsweredFromTheirOwnIndexes(t *testing.T) {
	w := newTwoRoots(t)
	h := NewContextHinter(w.storeFor, nil)
	ask := func(root, symbol string) HintResult {
		res, err := h.Hint(t.Context(), HintRequest{Workspace: root, Seeds: []ContextSeed{{Symbol: symbol}}, MaxBytes: 4096})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := ask(w.rootA, "OnlyInA"); len(res.Lines) == 0 || res.Lines[0].Path != "pricing/only_a.go" {
		t.Fatalf("control: root A's own function was not found: %+v", res)
	}
	res := ask(w.rootB, "OnlyInA")
	if len(res.Lines) != 0 || strings.Contains(hintText(res), "only_a.go") {
		t.Errorf("root B was answered from root A's index: %+v", res)
	}
	if res := ask(w.rootB, "OnlyInB"); len(res.Lines) == 0 || res.Lines[0].Path != "pricing/only_b.go" {
		t.Errorf("control: root B's own function was not found: %+v", res)
	}
	// A root the accessor has no index for is answered with a label, not with
	// another root's index.
	other := t.TempDir()
	none := ask(other, "OnlyInA")
	if len(none.Lines) != 0 || none.Freshness != "unavailable" || !slices.ContainsFunc(none.Gaps, func(g string) bool { return strings.Contains(g, "no topology index is available") }) {
		t.Errorf("a root with no index: %+v", none)
	}
}
