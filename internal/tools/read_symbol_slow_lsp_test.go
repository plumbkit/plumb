package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/topology"
)

// TestReadSymbol_SlowLSPFallsBackInsideBudget is the PLAN-390 guard, and it is
// the half that matters to real agents rather than only to CI.
//
// read_symbol advertises "falls back to a tree-sitter parse when the language
// server is cold or absent", and the pool is built to make that happen: a cold
// server is handed back not-yet-ready after firstStartGrace precisely "so the
// tool falls back to the tree-sitter index instead of blocking until the MCP
// client times out" (internal/cli/pool.go). A server that is merely SLOW —
// gopls on a cold module cache, the exact case the fallback exists for — used
// to defeat both halves of that: the LSP attempt was given the WHOLE [lsp_query]
// budget, and the fallback was then invoked on that same, already-expired
// context, which topology's safeExtract refuses to start a parse on. So the
// fallback could neither be reached in time nor run once reached.
//
// The tool must therefore answer from tree-sitter STRICTLY INSIDE its own
// budget. The elapsed-time assertion is the load-bearing one: a caller whose
// patience is the budget (an MCP client configured at the [lsp_query] timeout)
// must still see the fallback rather than a transport timeout.
func TestReadSymbol_SlowLSPFallsBackInsideBudget(t *testing.T) {
	store, _, uri := fallbackFixture(t)
	// A server that accepts the request and simply never answers — the limit
	// case of "cold and still indexing".
	slow := &mockLSP{block: true}
	const budget = 2 * time.Second
	tool := tools.NewReadSymbol(slow, nil, 0, budget, tools.NewReadTracker()).
		WithTopologyFallback(func() *topology.Store { return store })
	args, _ := json.Marshal(map[string]any{"path": uri, "name": "Beta"})

	start := time.Now()
	out, err := tool.Execute(context.Background(), args)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("a slow language server must degrade to the tree-sitter fallback, "+
			"got an error after %v: %v", elapsed, err)
	}
	for _, want := range []string{"topology fallback", "func Beta() int {", "return 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("slow-LSP fallback missing %q:\n%s", want, out)
		}
	}
	if elapsed >= budget {
		t.Errorf("fallback answered after %v — at or past the whole %v budget. "+
			"A caller whose patience equals the budget never sees it, which is the "+
			"PLAN-390 inversion; the LSP attempt must leave headroom for the parse.",
			elapsed, budget)
	}
}

// TestReadSymbol_SlowLSPErrorNamesTheAttempt keeps the timeout message honest
// when there is no fallback to reach: it must quote the budget actually waited,
// not the full [lsp_query] timeout, or the operator is told to expect a wait
// twice as long as the one that happened.
func TestReadSymbol_SlowLSPErrorNamesTheAttempt(t *testing.T) {
	_, _, uri := fallbackFixture(t)
	const budget = 2 * time.Second
	tool := tools.NewReadSymbol(&mockLSP{block: true}, nil, 0, budget, tools.NewReadTracker())
	args, _ := json.Marshal(map[string]any{"path": uri, "name": "Beta"})

	start := time.Now()
	_, err := tool.Execute(context.Background(), args)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error with no topology store wired")
	}
	if elapsed >= budget {
		t.Errorf("gave up after %v, at or past the whole %v budget", elapsed, budget)
	}
	if !strings.Contains(err.Error(), "did not respond within") {
		t.Fatalf("expected the friendly timeout message, got: %v", err)
	}
	if strings.Contains(err.Error(), budget.String()) {
		t.Errorf("timeout message quotes the full budget %v rather than the "+
			"attempt actually waited (~%v): %v", budget, elapsed, err)
	}
}
