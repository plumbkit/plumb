package cli

// conn_repin_report_race_test.go — concurrent re-pins must not produce false
// re-pin reports (#517 review). The connection's previous root used to be read
// before the mutation lane that moves it, so of several callers racing to the
// same target every one could report moving the pin, although only the first
// did; the rest were same-root no-ops.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

// Four callers ask for the same root at once, off a non-sticky (roots) pin.
// Exactly one move happens, so exactly one report may claim it (From != Root).
func TestRepinReport_ConcurrentSameTargetClaimsOneMove(t *testing.T) {
	const iters, callers = 50, 4
	for i := range iters {
		s := newRepinReportSession(t)
		r := repinReportRoots(t, 2)
		rootA, rootB := r[0], r[1]
		s.attachWorkspace(context.Background(), "file://"+rootA)
		var wg sync.WaitGroup
		claims := make([]bool, callers)
		start := make(chan struct{})
		for g := range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				rep, err := s.repinWorkspace(context.Background(), rootB, "", false, false)
				if err != nil {
					t.Errorf("repin: %v", err)
					return
				}
				claims[g] = rep.From != "" && rep.From != rep.Root
			}()
		}
		close(start)
		wg.Wait()
		n := 0
		for _, c := range claims {
			if c {
				n++
			}
		}
		// Positive control: the one real move must still be reported.
		if n != 1 {
			t.Fatalf("iteration %d: %d of %d concurrent re-pins to one target reported moving the pin, want exactly 1", i, n, callers)
		}
	}
}

// Concurrent session_starts from several identified agents, mixing agent and
// connection scope, through the real tool surface. It asserts nothing beyond
// completing: its value is under -race, over the new report paths.
func TestRepinReport_ConcurrentSessionStartsRace(t *testing.T) {
	s := newRepinReportSession(t)
	r := repinReportRoots(t, 3)
	if _, err := s.repinWorkspace(context.Background(), r[0], "", false, false); err != nil {
		t.Fatal(err)
	}
	ids := []string{"a", "b", "c"}
	for _, id := range ids {
		s.recordLogicalAgentAttach(id)
	}
	var wg sync.WaitGroup
	for i := range 30 {
		for _, id := range ids {
			wg.Add(1)
			go func() {
				defer wg.Done()
				args := map[string]any{"detail": "brief", "workspace": r[(i+len(id))%3], "force": true}
				if i%3 == 0 {
					args["scope"] = "connection"
				}
				raw, err := json.Marshal(args)
				if err != nil {
					t.Errorf("marshal: %v", err)
					return
				}
				// Refusals are legitimate under this contention; only races matter.
				_, _ = newSessionStartTool(s).Execute(mcp.WithLogicalAgent(context.Background(), id), raw)
			}()
		}
	}
	wg.Wait()
}
