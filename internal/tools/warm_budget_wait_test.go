package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// warm_budget_wait_test.go is the issue #483 guard for the two warm-path cases
// whose wall-clock bound used to live in slow_lsp_fallback_test.go and
// call_hierarchy_slow_lsp_test.go. See budgetWaitHook in lsp_deadline.go.
//
// The property is a MECHANISM: when the language server answers, the tool must
// return on that answer and must NOT block on a budget — neither the shorter
// language-server attempt nor the tool's own [lsp_query] budget, which the write
// path also runs under. Blocking on either goes through that context's Done
// channel, so the test counts reads of BOTH (they share one hook): exactly zero
// for a warm server. The slow control proves the counter is live (a server that
// answers nothing reads it), so a zero result is evidence rather than silence.
// No clock is measured, so a loaded runner cannot flake the assertion.

// waitCountingBudget installs a counting budgetWaitHook for the duration of
// the test and returns the running total. The hook is a package-level var, so
// the test must not run in parallel — the same contract as syncFileHook.
func waitCountingBudget(t *testing.T) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	orig := budgetWaitHook
	budgetWaitHook = func() { n.Add(1) }
	t.Cleanup(func() { budgetWaitHook = orig })
	return &n
}

// warmBudgetLSP answers immediately (warm) or blocks on the attempt budget until
// it expires (slow). The embedded interface supplies the methods these paths
// never reach; a call to one panics on the nil interface, which is the right
// failure for a test that does not expect it.
type warmBudgetLSP struct {
	lsp.Client
	warm bool
}

func (f *warmBudgetLSP) DocumentSymbols(ctx context.Context, _ protocol.DocumentSymbolParams) ([]protocol.DocumentSymbol, error) {
	if !f.warm {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return []protocol.DocumentSymbol{{
		Name: "Alpha",
		Kind: protocol.SKFunction,
		Range: protocol.Range{
			Start: protocol.Position{Line: 2, Character: 0},
			End:   protocol.Position{Line: 4, Character: 1},
		},
		SelectionRange: protocol.Range{
			Start: protocol.Position{Line: 2, Character: 5},
			End:   protocol.Position{Line: 2, Character: 8},
		},
	}}, nil
}

func (f *warmBudgetLSP) PrepareCallHierarchy(ctx context.Context, _ protocol.PrepareCallHierarchyParams) ([]protocol.CallHierarchyItem, error) {
	if !f.warm {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return []protocol.CallHierarchyItem{{
		Name:           "Mid",
		Kind:           protocol.SKFunction,
		URI:            "file:///demo.go",
		Range:          protocol.Range{Start: protocol.Position{Line: 4}},
		SelectionRange: protocol.Range{Start: protocol.Position{Line: 4, Character: 5}},
	}}, nil
}

func (f *warmBudgetLSP) IncomingCalls(_ context.Context, _ protocol.CallHierarchyIncomingCallsParams) ([]protocol.CallHierarchyIncomingCall, error) {
	return []protocol.CallHierarchyIncomingCall{{From: protocol.CallHierarchyItem{
		Name: "ServerSaysTop", Kind: protocol.SKFunction, URI: "file:///demo.go",
	}}}, nil
}

// warmSymbolEdit drives the symbol-edit warm path through the real tool: the
// resolver closure inside ReplaceSymbolBody.Execute is the site the wall-clock
// bound used to guard.
func warmSymbolEdit(t *testing.T, warm bool) (string, error) {
	t.Helper()
	tool := NewReplaceSymbolBody(&warmBudgetLSP{warm: warm}, 20*time.Millisecond)
	args, err := json.Marshal(map[string]any{
		"uri": "file:///demo.go", "name_path": "Alpha", "content": "x", "dry_run": true,
	})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return tool.Execute(context.Background(), args)
}

// warmCallHierarchy drives call_hierarchy the same way; PrepareCallHierarchy is
// its attempt-budget site.
func warmCallHierarchy(t *testing.T, warm bool) (string, error) {
	t.Helper()
	tool := NewCallHierarchy(&warmBudgetLSP{warm: warm}, 20*time.Millisecond)
	args, err := json.Marshal(map[string]any{
		"uri": "file:///demo.go", "line": 4, "character": 5, "direction": "incoming",
	})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return tool.Execute(context.Background(), args)
}

func TestWarmPath_NeverWaitsOnBudget(t *testing.T) {
	drivers := []struct {
		name string
		run  func(t *testing.T, warm bool) (string, error)
		// warmMark is a token only the SERVER's answer can contain. Asserting it
		// stops "zero waits" from passing because the path failed before it ever
		// reached the server.
		warmMark string
	}{
		{"replace_symbol_body", warmSymbolEdit, "Range: line 2 char 0"},
		{"call_hierarchy", warmCallHierarchy, "ServerSaysTop"},
	}

	for _, d := range drivers {
		t.Run(d.name+"/warm answers with no budget wait", func(t *testing.T) {
			waits := waitCountingBudget(t)
			out, err := d.run(t, true)
			if err != nil {
				t.Fatalf("warm server: %v", err)
			}
			if !strings.Contains(out, d.warmMark) {
				t.Fatalf("the server's answer did not reach the response (want %q):\n%s", d.warmMark, out)
			}
			if strings.Contains(out, "topology fallback") || strings.Contains(out, "reconstructed") {
				t.Errorf("an answering server must own the answer, with no topology banner:\n%s", out)
			}
			if n := waits.Load(); n != 0 {
				t.Errorf("the warm path read a budget context's Done channel %d time(s); when the "+
					"server answers the path must end on that answer, never on a budget timer", n)
			}
		})

		t.Run(d.name+"/slow control does wait", func(t *testing.T) {
			waits := waitCountingBudget(t)
			// The result is expected to be an error or a fallback; only the read
			// of the budget matters here.
			_, _ = d.run(t, false)
			if waits.Load() == 0 {
				t.Fatal("a server that never answers did not read a budget context's Done " +
					"channel, so this counter cannot observe a wait and the warm assertion is vacuous")
			}
		})
	}

	// The slow control's liveness comes from the fake CLIENT reading the attempt
	// context, so it defends only that half. Without this control, dropping
	// observeBudgetWait(rawToolCtx) would leave the whole suite green and silently
	// revert the guard to the attempt context alone.
	t.Run("tool budget is observed", func(t *testing.T) {
		waits := waitCountingBudget(t)
		toolCtx, _, cancel, _ := fallbackDeadlines(context.Background(), 20*time.Millisecond)
		defer cancel()
		_ = toolCtx.Done()
		if waits.Load() == 0 {
			t.Fatal("the tool budget is not observed: dropping its wrapper would silently revert " +
				"the warm guard to the attempt context alone")
		}
	})
}
