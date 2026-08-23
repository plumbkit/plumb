package tools_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/topology"
)

// call_hierarchy_slow_lsp_test.go covers the EIGHTH site of the PLAN-403 defect,
// which the card's affected list did not name (review §3). call_hierarchy is the
// one of the four position-taking query tools wired with a topology fallback,
// and it shared executeLSPQuery's single-context skeleton: ctx was shadowed by
// the LSP deadline, so when PrepareCallHierarchy missed that deadline the
// fallback ran on a dead context (topology's safeExtract refuses to start a
// parse on one) with no headroom left — and the tool returned the very timeout
// whose error text advertises the index that could have answered.
//
// The fixture is newCallGraphStore (call_hierarchy_test.go): Top → Mid → Bottom,
// with Mid declared on 0-based line 4.

// midDeclLine is Mid's 0-based declaration line in newCallGraphStore's fixture.
const midDeclLine = 4

func TestCallHierarchy_SlowLSPAnswersFromTopologyInsideBudget(t *testing.T) {
	for _, parent := range parentContexts() {
		t.Run(parent.name, func(t *testing.T) {
			store, uri := newCallGraphStore(t)
			tool := tools.NewCallHierarchy(slowLSP(), slowFallbackBudget).
				WithTopologyFallback(func() *topology.Store { return store })
			args := slowFallbackArgs(t, map[string]any{
				"uri": uri, "line": midDeclLine, "character": 5, "direction": "both",
			})
			// The caller's deadline starts AFTER the fixture is built, so the
			// case runs the budget it names rather than what setup left.
			parentCtx := parent.ctx(t)

			start := time.Now()
			out, err := tool.Execute(parentCtx, args)
			elapsed := time.Since(start)

			if err != nil {
				t.Fatalf("a slow language server must degrade to the topology call graph; "+
					"got an error after %v: %v", elapsed, err)
			}
			if elapsed >= slowFallbackBudget {
				t.Errorf("answered after %v — at or past the whole %v budget, so a caller whose "+
					"patience equals it sees a timeout instead of the reconstructed hierarchy",
					elapsed, slowFallbackBudget)
			}
			// Provenance, and both directions: a reached-but-dead fallback
			// returns ok=false and renders none of this.
			if !strings.Contains(out, "reconstructed") {
				t.Errorf("expected the reconstructed (topology) hierarchy:\n%s", out)
			}
			if !strings.Contains(out, "Top") {
				t.Errorf("expected Top as the incoming caller of Mid:\n%s", out)
			}
			if !strings.Contains(out, "Bottom") {
				t.Errorf("expected Bottom as the outgoing callee of Mid:\n%s", out)
			}
		})
	}
}

// TestCallHierarchy_WarmLSPUnchanged is the direction an over-eager fix breaks:
// once the server HAS resolved an item the fallback is out of the picture, so
// the incoming/outgoing follow-ups keep the full tool budget and the output is
// the server's, with no topology banner.
func TestCallHierarchy_WarmLSPUnchanged(t *testing.T) {
	store, uri := newCallGraphStore(t)
	client := &mockLSP{
		chItems: []protocol.CallHierarchyItem{{
			Name: "Mid", Kind: protocol.SKFunction, URI: uri,
			Range:          protocol.Range{Start: protocol.Position{Line: midDeclLine}},
			SelectionRange: protocol.Range{Start: protocol.Position{Line: midDeclLine, Character: 5}},
		}},
		chIncoming: []protocol.CallHierarchyIncomingCall{{From: protocol.CallHierarchyItem{
			Name: "ServerSaysTop", Kind: protocol.SKFunction, URI: uri,
		}}},
	}
	tool := tools.NewCallHierarchy(client, slowFallbackBudget).
		WithTopologyFallback(func() *topology.Store { return store })
	args := slowFallbackArgs(t, map[string]any{
		"uri": uri, "line": midDeclLine, "character": 5, "direction": "incoming",
	})

	start := time.Now()
	out, err := tool.Execute(context.Background(), args)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("warm server: %v", err)
	}
	if strings.Contains(out, "reconstructed") {
		t.Errorf("an answering server must own the hierarchy, with no topology banner:\n%s", out)
	}
	if !strings.Contains(out, "ServerSaysTop") {
		t.Errorf("expected the server's own callers, not the index's:\n%s", out)
	}
	if elapsed > slowFallbackBudget/20 {
		t.Errorf("warm path took %v; bounding the attempt must add no latency when the "+
			"server answers", elapsed)
	}
}
